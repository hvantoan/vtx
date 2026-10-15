package application

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/persistence/db"
)

// Receipt is one append-only webhook_receipts row: proof that an
// authenticated request reached the intake, with its settled outcome.
// Written AFTER processing, best-effort — a write failure must never change
// the HTTP response or roll back the acceptance transaction.
type Receipt struct {
	ReceivedAt  time.Time
	Outcome     string // outcome vocabulary: dispositions + rejected/failed_retryable
	ReasonCode  string
	RequestID   string
	PayloadHash string // sha256 of what was read (truncated bodies hash truncated input)
	IssueKey    string // empty for transport rejects / missing identity
	ErrorDetail string // optional; never credentials or raw payload
}

// Outcome values used only by receipts (not inbox CHECK-listed).
const (
	DispositionRejected        = "rejected"
	DispositionFailedRetryable = "failed_retryable"
)

// Additional receipt reason codes for transport-level rejects.
const (
	ReasonBadContentType  = "bad_content_type"
	ReasonMalformedJSON   = "malformed_json"
	ReasonPayloadTooLarge = "payload_too_large"
	ReasonBodyReadFailure = "body_read_failure"
	ReasonInternalError   = "internal_error"
)

// ReceiptRetentionDays is how long terminal noise receipts are kept.
const ReceiptRetentionDays = 7

// ReceiptRecorder persists receipts via sqlc. Best-effort by contract: the
// caller logs failures but proceeds.
type ReceiptRecorder struct {
	queries *db.Queries
}

func NewReceiptRecorder(queries *db.Queries) *ReceiptRecorder {
	return &ReceiptRecorder{queries: queries}
}

func (r *ReceiptRecorder) Record(ctx context.Context, rec Receipt) error {
	_, err := r.queries.InsertWebhookReceipt(ctx, db.InsertWebhookReceiptParams{
		ReceivedAt:  pgtype.Timestamptz{Time: rec.ReceivedAt, Valid: true},
		Outcome:     rec.Outcome,
		ReasonCode:  textParam(rec.ReasonCode),
		RequestID:   textParam(rec.RequestID),
		PayloadHash: textParam(rec.PayloadHash),
		IssueKey:    textParam(rec.IssueKey),
		ErrorDetail: textParam(rec.ErrorDetail),
	})
	return err
}

// DeleteExpiredReceipts applies the 7-day retention on terminal noise
// outcomes; accepted/duplicate rows are kept as the audit record.
func (r *ReceiptRecorder) DeleteExpiredReceipts(ctx context.Context, days int32) (int64, error) {
	return r.queries.DeleteExpiredWebhookReceipts(ctx, days)
}
