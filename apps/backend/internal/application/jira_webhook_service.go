package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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

// Outbox contract consumed by VTX-3 downstream processing.
const OutboxEventExecutionIntentCreated = "execution_intent.created.v1"

type Result struct {
	Disposition string
	IntentID    string // set for accepted and duplicate dispositions
}

// TxBeginner is satisfied by *pgxpool.Pool.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type JiraWebhookService struct {
	cfg     *config.Config
	pool    TxBeginner
	queries *db.Queries
	jira    jira.StatusChecker
	logger  *slog.Logger
}

func NewJiraWebhookService(cfg *config.Config, pool TxBeginner, queries *db.Queries, jiraClient jira.StatusChecker, logger *slog.Logger) *JiraWebhookService {
	return &JiraWebhookService{cfg: cfg, pool: pool, queries: queries, jira: jiraClient, logger: logger}
}

// ProcessWebhookTransition runs intake + dedup for one webhook delivery.
//
// Ordering matters: every terminal ignorable outcome writes an inbox row for
// auditability, while retryable failures return ErrRetryable BEFORE any write
// so the dedup key is never claimed and Jira redelivery reprocesses cleanly.
func (s *JiraWebhookService) ProcessWebhookTransition(ctx context.Context, env *domain.JiraWebhookEnvelope, rawPayload []byte, requestID string) (Result, error) {
	// --- Envelope gates: ignorable traffic acknowledged without a DB row ---
	if env.WebhookEvent != "jira:issue_updated" {
		return Result{Disposition: DispositionIgnoredScope}, nil
	}
	if env.Changelog == nil || env.Changelog.ID == "" {
		return Result{Disposition: DispositionIgnoredScope}, nil
	}
	targetStatusID, ok := env.StatusTransitionTo()
	if !ok {
		// Non-status issue_updated (comment, assignee, labels — the bulk of
		// Jira traffic). 202 only; penalizing it gets the webhook disabled.
		return Result{Disposition: DispositionIgnoredScope}, nil
	}
	if env.Issue == nil || env.Issue.ID == "" || env.Issue.Key == "" {
		return Result{Disposition: DispositionIgnoredScope}, nil
	}
	if err := jira.ValidateIssueID(env.Issue.ID); err != nil {
		return Result{Disposition: DispositionIgnoredScope}, nil
	}
	if err := jira.ValidateIssueKey(env.Issue.Key); err != nil {
		return Result{Disposition: DispositionIgnoredScope}, nil
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

	// --- Duplicate check first: redelivery of a claimed key returns the
	// existing intent without touching Jira or starting a write tx.
	if existing, err := s.queries.GetJiraWebhookInboxByEventKey(ctx, eventKey); err == nil {
		return Result{Disposition: DispositionDuplicate, IntentID: uuidString(existing.IntentID)}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("%w: inbox lookup: %v", jira.ErrRetryable, err)
	}

	// --- Scope gate: project allowlist (payload field, cheap pre-check).
	if !s.projectAllowed(projectKey) {
		if err := s.insertTerminalInbox(ctx, eventKey, env.Issue.Key, DispositionIgnoredScope, hashHex, requestID); err != nil {
			return Result{}, err
		}
		return Result{Disposition: DispositionIgnoredScope}, nil
	}

	// --- Authority re-fetch: the live Jira API is the only truth about the
	// current status. Webhook payload fields are hints, never proof.
	currentStatus, err := s.jira.FetchIssueStatus(ctx, env.Issue.ID)
	if err != nil {
		// Retryable path: return BEFORE any inbox insert so the event key
		// remains unclaimed for Jira's redelivery.
		return Result{}, fmt.Errorf("jira re-fetch failed: %w", err)
	}
	if currentStatus != s.cfg.JiraReadyToDevStatusID {
		if err := s.insertTerminalInbox(ctx, eventKey, env.Issue.Key, DispositionIgnoredStale, hashHex, requestID); err != nil {
			return Result{}, err
		}
		return Result{Disposition: DispositionIgnoredStale}, nil
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

	outboxPayload, err := json.Marshal(map[string]any{
		"intent_id":        intentID,
		"issue_key":        env.Issue.Key,
		"issue_id":         env.Issue.ID,
		"jira_site_id":     s.cfg.JiraSiteID,
		"changelog_id":     env.Changelog.ID,
		"target_status_id": targetStatusID,
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

func (s *JiraWebhookService) projectAllowed(projectKey string) bool {
	for _, allowed := range s.cfg.JiraProjectAllowlist {
		if allowed == projectKey {
			return true
		}
	}
	return false
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
