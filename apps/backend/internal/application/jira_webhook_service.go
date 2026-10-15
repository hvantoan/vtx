package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/persistence/db"
)

// Dispositions surfaced to the HTTP layer and persisted as inbox outcomes.
// "duplicate" is a response-level disposition — the stored row keeps its
// original terminal outcome.
const (
	DispositionAccepted          = "accepted"
	DispositionDuplicate         = "duplicate"
	DispositionIgnoredStale      = "ignored_stale"
	DispositionIgnoredScope      = "ignored_scope"
	DispositionInvalidTransition = "invalid_transition"
)

// Reason codes persisted on webhook_receipts rows (Phase 6) and returned in
// Result so the transport layer can audit every authenticated request.
const (
	ReasonUnsupportedEvent  = "unsupported_event"
	ReasonMissingChangelog  = "missing_changelog"
	ReasonNonStatusChange   = "non_status_change"
	ReasonMissingIssue      = "missing_issue_identity"
	ReasonIssueIDFormat     = "issue_id_format"
	ReasonIssueKeyFormat    = "issue_key_format"
	ReasonProjectNotAllowed = "project_not_allowlisted"
	ReasonTargetNotStart    = "target_not_start_status"
	ReasonIdentityMismatch  = "identity_mismatch"
	ReasonStatusDrift       = "status_drift"
)

// Outbox contract consumed by VTX-3 downstream processing.
const OutboxEventExecutionIntentCreated = "execution_intent.created.v1"

type Result struct {
	Disposition string
	IntentID    string // set for accepted and duplicate dispositions
	ReasonCode  string // populated for rejected/ignored dispositions (audit)
}

// TxBeginner is satisfied by *pgxpool.Pool.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type JiraWebhookService struct {
	cfg     *config.Config
	pool    TxBeginner
	queries *db.Queries
	jira    jira.IssueFetcher
	logger  *slog.Logger
}

func NewJiraWebhookService(cfg *config.Config, pool TxBeginner, queries *db.Queries, jiraClient jira.IssueFetcher, logger *slog.Logger) *JiraWebhookService {
	return &JiraWebhookService{cfg: cfg, pool: pool, queries: queries, jira: jiraClient, logger: logger}
}

// ProcessWebhookTransition runs intake + dedup for one webhook delivery.
//
// Ordering matters:
//  1. Envelope gates reject non-status traffic without touching the DB.
//  2. The transition-target gate runs BEFORE the duplicate lookup: a
//     transition whose `to` status is not a configured start status is a
//     receipt-only `invalid_transition` — it must never claim a dedup key
//     (review P1-1: forged targets previously reached the fetch path).
//  3. Identity is re-verified against the live Jira API — webhook payload
//     fields are hints, never proof (review P1-2).
//  4. Retryable failures return BEFORE any write so the dedup key stays
//     unclaimed and Jira redelivery reprocesses cleanly.
func (s *JiraWebhookService) ProcessWebhookTransition(ctx context.Context, env *domain.JiraWebhookEnvelope, rawPayload []byte, requestID string, receivedAt time.Time) (Result, error) {
	// --- Envelope gates: ignorable traffic acknowledged without a DB row ---
	if env.WebhookEvent != "jira:issue_updated" {
		return Result{Disposition: DispositionIgnoredScope, ReasonCode: ReasonUnsupportedEvent}, nil
	}
	if env.Changelog == nil || env.Changelog.ID == "" {
		return Result{Disposition: DispositionIgnoredScope, ReasonCode: ReasonMissingChangelog}, nil
	}
	targetStatusID, ok := env.StatusTransitionTo()
	if !ok {
		// Non-status issue_updated (comment, assignee, labels — the bulk of
		// Jira traffic). 202 only; penalizing it gets the webhook disabled.
		return Result{Disposition: DispositionIgnoredScope, ReasonCode: ReasonNonStatusChange}, nil
	}
	if env.Issue == nil || env.Issue.ID == "" || env.Issue.Key == "" {
		return Result{Disposition: DispositionIgnoredScope, ReasonCode: ReasonMissingIssue}, nil
	}
	if err := jira.ValidateIssueID(env.Issue.ID); err != nil {
		return Result{Disposition: DispositionIgnoredScope, ReasonCode: ReasonIssueIDFormat}, nil
	}
	if err := jira.ValidateIssueKey(env.Issue.Key); err != nil {
		return Result{Disposition: DispositionIgnoredScope, ReasonCode: ReasonIssueKeyFormat}, nil
	}

	payloadHash := sha256.Sum256(rawPayload)
	hashHex := hex.EncodeToString(payloadHash[:])

	eventKey := db.GetJiraWebhookInboxByEventKeyParams{
		JiraSiteID:     s.cfg.JiraSiteID,
		IssueID:        env.Issue.ID,
		ChangelogID:    env.Changelog.ID,
		TargetStatusID: targetStatusID,
	}

	projectKey := env.Issue.Fields.Project.Key

	// --- Scope gate: project must be configured. Terminal inbox row (fully
	// identified event) — redeliveries dedup to the original outcome.
	startIDs, ok := s.cfg.StartStatusIDsFor(projectKey)
	if !ok {
		if err := s.insertTerminalInbox(ctx, eventKey, env.Issue.Key, DispositionIgnoredScope, hashHex, requestID); err != nil {
			return Result{}, err
		}
		return Result{Disposition: DispositionIgnoredScope, ReasonCode: ReasonProjectNotAllowed}, nil
	}

	// --- Transition-target gate BEFORE dedup: only transitions INTO a
	// configured start status may claim an event key or produce an intent.
	// Wrong-target events are receipts-only — no inbox row, no Jira call.
	if !slices.Contains(startIDs, targetStatusID) {
		s.logger.Warn("jira webhook transition target not a start status",
			slog.String("request_id", requestID),
			slog.String("issue_key", env.Issue.Key),
			slog.String("project_key", projectKey),
			slog.String("target_status_id", targetStatusID))
		return Result{Disposition: DispositionInvalidTransition, ReasonCode: ReasonTargetNotStart}, nil
	}

	// --- Duplicate check: redelivery of a claimed key returns the existing
	// intent without touching Jira or starting a write tx.
	if existing, err := s.queries.GetJiraWebhookInboxByEventKey(ctx, eventKey); err == nil {
		return Result{Disposition: DispositionDuplicate, IntentID: uuidString(existing.IntentID)}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("%w: inbox lookup: %v", jira.ErrRetryable, err)
	}

	// --- Authority re-fetch: the live Jira API is the only truth about issue
	// identity and current status. Webhook payload fields are hints, never
	// proof — a forged payload claiming project VTX for an OTHER-project
	// issue is caught here.
	fetched, err := s.jira.FetchIssue(ctx, env.Issue.ID)
	if err != nil {
		// Retryable path: return BEFORE any inbox insert so the event key
		// remains unclaimed for Jira's redelivery.
		return Result{}, fmt.Errorf("jira re-fetch failed: %w", err)
	}
	if fetched.ID != env.Issue.ID || fetched.ProjectKey != projectKey {
		s.logger.Warn("jira webhook identity mismatch — forged or corrupt payload",
			slog.String("request_id", requestID),
			slog.String("claimed_issue_id", env.Issue.ID),
			slog.String("claimed_project", projectKey),
			slog.String("fetched_issue_id", fetched.ID),
			slog.String("fetched_project", fetched.ProjectKey))
		if err := s.insertTerminalInbox(ctx, eventKey, env.Issue.Key, DispositionIgnoredScope, hashHex, requestID); err != nil {
			return Result{}, err
		}
		return Result{Disposition: DispositionIgnoredScope, ReasonCode: ReasonIdentityMismatch}, nil
	}
	if !slices.Contains(startIDs, fetched.StatusID) {
		if err := s.insertTerminalInbox(ctx, eventKey, env.Issue.Key, DispositionIgnoredStale, hashHex, requestID); err != nil {
			return Result{}, err
		}
		return Result{Disposition: DispositionIgnoredStale, ReasonCode: ReasonStatusDrift}, nil
	}

	// --- Accepted path: exactly one pgx.Tx binds all four statements.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("%w: begin tx: %v", jira.ErrRetryable, err)
	}
	defer tx.Rollback(ctx) // no-op after Commit

	qtx := s.queries.WithTx(tx)

	inbox, err := qtx.InsertJiraWebhookInbox(ctx, db.InsertJiraWebhookInboxParams{
		JiraSiteID:     eventKey.JiraSiteID,
		IssueID:        eventKey.IssueID,
		IssueKey:       env.Issue.Key,
		ChangelogID:    eventKey.ChangelogID,
		TargetStatusID: eventKey.TargetStatusID,
		Outcome:        DispositionAccepted,
		PayloadHash:    hashHex,
		RequestID:      textParam(requestID),
		CorrelationID:  textParam(requestID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the insert race: another worker claimed the key. Read the
			// winner's row (outside this tx — it rolls back) and report its
			// intent so the response is stable across redeliveries.
			tx.Rollback(ctx)
			winner, lookupErr := s.queries.GetJiraWebhookInboxByEventKey(ctx, eventKey)
			if lookupErr != nil {
				return Result{}, fmt.Errorf("%w: duplicate lookup: %v", jira.ErrRetryable, lookupErr)
			}
			return Result{Disposition: DispositionDuplicate, IntentID: uuidString(winner.IntentID)}, nil
		}
		return Result{}, fmt.Errorf("%w: insert inbox: %v", jira.ErrRetryable, err)
	}

	intent, err := qtx.InsertExecutionIntent(ctx, db.InsertExecutionIntentParams{
		Source:         "jira_webhook",
		IssueKey:       env.Issue.Key,
		TargetStatusID: targetStatusID,
	})
	if err != nil {
		return Result{}, fmt.Errorf("%w: insert intent: %v", jira.ErrRetryable, err)
	}
	intentID := uuidString(intent.IntentID)

	occurredAt, occurredSource := OccurredAt(env, receivedAt)
	outboxPayload, err := json.Marshal(ExecutionIntentCreatedV1{
		EventID:          uuidString(inbox.EventID),
		IntentID:         intentID,
		IssueID:          env.Issue.ID,
		IssueKey:         env.Issue.Key,
		JiraSiteID:       s.cfg.JiraSiteID,
		ChangelogID:      env.Changelog.ID,
		TargetStatusID:   targetStatusID,
		OccurredAt:       occurredAt.UTC().Format(time.RFC3339),
		OccurredAtSource: occurredSource,
		CorrelationID:    requestID,
	})
	if err != nil {
		return Result{}, fmt.Errorf("marshal outbox payload: %w", err)
	}
	if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
		IntentID:  intent.IntentID,
		EventType: OutboxEventExecutionIntentCreated,
		Payload:   outboxPayload,
	}); err != nil {
		return Result{}, fmt.Errorf("%w: insert outbox: %v", jira.ErrRetryable, err)
	}

	if err := qtx.UpdateJiraWebhookInboxOutcome(ctx, db.UpdateJiraWebhookInboxOutcomeParams{
		EventID:  inbox.EventID,
		IntentID: intent.IntentID,
		Outcome:  DispositionAccepted,
	}); err != nil {
		return Result{}, fmt.Errorf("%w: update inbox outcome: %v", jira.ErrRetryable, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("%w: commit: %v", jira.ErrRetryable, err)
	}

	return Result{Disposition: DispositionAccepted, IntentID: intentID}, nil
}

// insertTerminalInbox persists an ignorable (non-accepted) terminal outcome.
// These rows claim the dedup key deliberately — a stale or out-of-scope
// redelivery stays deduplicated instead of re-hitting the Jira API. A
// constraint-violating outcome (e.g. invalid_transition semantics) must never
// be written: only CHECK-listed terminal values are passed in.
func (s *JiraWebhookService) insertTerminalInbox(ctx context.Context, key db.GetJiraWebhookInboxByEventKeyParams, issueKey, outcome, payloadHash, requestID string) error {
	_, err := s.queries.InsertJiraWebhookInbox(ctx, db.InsertJiraWebhookInboxParams{
		JiraSiteID:     key.JiraSiteID,
		IssueID:        key.IssueID,
		IssueKey:       issueKey,
		ChangelogID:    key.ChangelogID,
		TargetStatusID: key.TargetStatusID,
		Outcome:        outcome,
		PayloadHash:    payloadHash,
		RequestID:      textParam(requestID),
		CorrelationID:  textParam(requestID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Another worker claimed the key concurrently — already terminal.
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: insert terminal inbox: %v", jira.ErrRetryable, err)
	}
	return nil
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	// pgtype.UUID stores the raw 16 bytes; format canonically.
	b := u.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func textParam(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
