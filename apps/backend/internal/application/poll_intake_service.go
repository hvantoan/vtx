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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/persistence/db"
)

// Poll-intake dispositions. They mirror the webhook vocabulary where the
// meaning is the same; the poller only ever sees these strings.
const (
	PollDispositionAccepted     = "accepted"
	PollDispositionDuplicate    = "duplicate"
	PollDispositionIgnoredStale = "ignored_stale"
	PollDispositionIgnoredScope = "ignored_scope"
)

// Poll-intake reason codes.
const (
	PollReasonProjectNotAllowed = "project_not_allowed"
	PollReasonStatusDrift       = "status_drift"
	PollReasonIdentityMismatch  = "identity_mismatch"
)

// PollIntakeRequest is one queued Jira issue from the master poller. Version is
// the poller's dedupe token (statuscategorychangedate, else updated); it never
// authorizes anything — the live Jira fetch does.
type PollIntakeRequest struct {
	IssueID       string `json:"issue_id"`
	IssueKey      string `json:"issue_key"`
	Version       string `json:"version"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// PollIntakeResult tells the poller what happened. Accepted and Duplicate carry
// an intent id; ignored outcomes carry a stable reason code for logs.
type PollIntakeResult struct {
	Disposition string `json:"disposition"`
	IntentID    string `json:"intent_id,omitempty"`
	IssueKey    string `json:"issue_key"`
	ReasonCode  string `json:"reason_code,omitempty"`
}

// PollIntakeService turns a polled Jira issue into an execution intent using
// the same authority rules as the webhook path: the live Jira API is the only
// truth about identity and status, and a project/status that is not configured
// can never produce an intent.
//
// Dedupe reuses jira_webhook_inbox with a synthetic changelog id derived from
// the poller version. That keeps one durable claim per (issue, version) without
// a new table; the intent's source is "jira_poll" so audits can tell the two
// intake paths apart.
type PollIntakeService struct {
	cfg     *config.Config
	pool    TxBeginner
	queries *db.Queries
	jira    jira.IssueFetcher
	clock   Clock
	logger  *slog.Logger
}

func NewPollIntakeService(cfg *config.Config, pool TxBeginner, queries *db.Queries, fetcher jira.IssueFetcher, clock Clock, logger *slog.Logger) *PollIntakeService {
	if clock == nil {
		clock = SystemClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PollIntakeService{cfg: cfg, pool: pool, queries: queries, jira: fetcher, clock: clock, logger: logger}
}

// pollChangelogID derives the synthetic, bounded dedupe key. 32 chars is the
// column limit, so the hash is truncated rather than the raw version string.
func pollChangelogID(version string) string {
	sum := sha256.Sum256([]byte(version))
	return "poll-" + hex.EncodeToString(sum[:])[:26]
}

func pollPayloadHash(issueID, version string) string {
	sum := sha256.Sum256([]byte(issueID + "\n" + version))
	return hex.EncodeToString(sum[:])
}

// Intake validates and persists one polled issue. Retryable upstream failures
// are returned as errors (the poller retries next tick); policy rejections are
// returned as results so a stale ticket does not look like a broken poller.
func (s *PollIntakeService) Intake(ctx context.Context, req PollIntakeRequest) (PollIntakeResult, error) {
	req.IssueID = strings.TrimSpace(req.IssueID)
	req.IssueKey = strings.TrimSpace(req.IssueKey)
	req.Version = strings.TrimSpace(req.Version)
	result := PollIntakeResult{IssueKey: req.IssueKey}

	if err := jira.ValidateIssueID(req.IssueID); err != nil {
		return result, fmt.Errorf("%w: %v", ErrPayloadInvalid, err)
	}
	if err := jira.ValidateIssueKey(req.IssueKey); err != nil {
		return result, fmt.Errorf("%w: %v", ErrPayloadInvalid, err)
	}
	if req.Version == "" {
		return result, fmt.Errorf("%w: version is required", ErrPayloadInvalid)
	}
	projectKey, err := projectKeyFromIssueKey(req.IssueKey)
	if err != nil {
		return result, err
	}

	// Scope gate before any Jira call: an unconfigured project is data, not work.
	startIDs, ok := s.cfg.StartStatusIDsFor(projectKey)
	if !ok {
		return PollIntakeResult{Disposition: PollDispositionIgnoredScope, IssueKey: req.IssueKey, ReasonCode: PollReasonProjectNotAllowed}, nil
	}

	fetched, err := s.jira.FetchIssue(ctx, req.IssueID)
	if err != nil {
		return result, fmt.Errorf("poll intake jira re-fetch: %w", err)
	}
	if fetched.ID != req.IssueID || fetched.Key != req.IssueKey || fetched.ProjectKey != projectKey {
		s.logger.Warn("poll intake identity mismatch — forged or corrupt queue entry",
			slog.String("claimed_issue_id", req.IssueID),
			slog.String("claimed_issue_key", req.IssueKey),
			slog.String("fetched_issue_id", fetched.ID),
			slog.String("fetched_issue_key", fetched.Key))
		return PollIntakeResult{Disposition: PollDispositionIgnoredScope, IssueKey: req.IssueKey, ReasonCode: PollReasonIdentityMismatch}, nil
	}
	// The queue entry is only a hint: the issue must still be in a configured
	// start status NOW, otherwise the ticket moved on and must not run.
	if !slices.Contains(startIDs, fetched.StatusID) {
		s.logger.Info("poll intake skipped: status drifted",
			slog.String("issue_key", fetched.Key),
			slog.String("current_status_id", fetched.StatusID))
		return PollIntakeResult{Disposition: PollDispositionIgnoredStale, IssueKey: fetched.Key, ReasonCode: PollReasonStatusDrift}, nil
	}

	changelogID := pollChangelogID(req.Version)
	eventKey := db.GetJiraWebhookInboxByEventKeyParams{
		JiraSiteID:     s.cfg.JiraSiteID,
		IssueID:        fetched.ID,
		ChangelogID:    changelogID,
		TargetStatusID: fetched.StatusID,
	}

	if existing, lookupErr := s.queries.GetJiraWebhookInboxByEventKey(ctx, eventKey); lookupErr == nil {
		return PollIntakeResult{Disposition: PollDispositionDuplicate, IntentID: uuidString(existing.IntentID), IssueKey: fetched.Key}, nil
	} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return result, fmt.Errorf("poll intake inbox lookup: %w", lookupErr)
	}

	correlationID := strings.TrimSpace(req.CorrelationID)
	if correlationID == "" {
		correlationID = "poll:" + fetched.Key + ":" + req.Version
	}
	if len(correlationID) > 128 {
		correlationID = correlationID[:128]
	}

	result, err = s.accept(ctx, fetched, changelogID, pollPayloadHash(fetched.ID, req.Version), correlationID)
	if err != nil {
		return result, err
	}
	s.logger.Info("poll intent created",
		"metric", "poll_intake",
		"issue_key", fetched.Key,
		"intent_id", result.IntentID,
		"disposition", result.Disposition,
		"correlation_id", correlationID)
	return result, nil
}

func (s *PollIntakeService) accept(ctx context.Context, issue jira.FetchedIssue, changelogID, payloadHash, correlationID string) (PollIntakeResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PollIntakeResult{}, fmt.Errorf("%w: begin tx: %v", jira.ErrRetryable, err)
	}
	defer tx.Rollback(ctx) // no-op after Commit

	qtx := s.queries.WithTx(tx)
	eventKey := db.GetJiraWebhookInboxByEventKeyParams{
		JiraSiteID:     s.cfg.JiraSiteID,
		IssueID:        issue.ID,
		ChangelogID:    changelogID,
		TargetStatusID: issue.StatusID,
	}

	inbox, err := qtx.InsertJiraWebhookInbox(ctx, db.InsertJiraWebhookInboxParams{
		JiraSiteID:     eventKey.JiraSiteID,
		IssueID:        eventKey.IssueID,
		IssueKey:       issue.Key,
		ChangelogID:    eventKey.ChangelogID,
		TargetStatusID: eventKey.TargetStatusID,
		Outcome:        PollDispositionAccepted,
		PayloadHash:    payloadHash,
		RequestID:      textParam("poll"),
		CorrelationID:  textParam(correlationID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Lost the insert race: read the winner outside this (rolled back) tx.
		tx.Rollback(ctx)
		winner, lookupErr := s.queries.GetJiraWebhookInboxByEventKey(ctx, eventKey)
		if lookupErr != nil {
			return PollIntakeResult{}, fmt.Errorf("%w: duplicate lookup: %v", jira.ErrRetryable, lookupErr)
		}
		return PollIntakeResult{Disposition: PollDispositionDuplicate, IntentID: uuidString(winner.IntentID), IssueKey: issue.Key}, nil
	}
	if err != nil {
		return PollIntakeResult{}, fmt.Errorf("%w: insert inbox: %v", jira.ErrRetryable, err)
	}

	intent, err := qtx.InsertExecutionIntent(ctx, db.InsertExecutionIntentParams{
		Source:         "jira_poll",
		IssueKey:       issue.Key,
		TargetStatusID: issue.StatusID,
	})
	if err != nil {
		return PollIntakeResult{}, fmt.Errorf("%w: insert intent: %v", jira.ErrRetryable, err)
	}
	intentID := uuidString(intent.IntentID)

	payload, err := json.Marshal(ExecutionIntentCreatedV1{
		EventID:          uuidString(inbox.EventID),
		IntentID:         intentID,
		IssueID:          issue.ID,
		IssueKey:         issue.Key,
		JiraSiteID:       s.cfg.JiraSiteID,
		ChangelogID:      changelogID,
		TargetStatusID:   issue.StatusID,
		OccurredAt:       s.clock.Now().UTC().Format(time.RFC3339),
		OccurredAtSource: OccurredSourceReceivedAt,
		CorrelationID:    correlationID,
	})
	if err != nil {
		return PollIntakeResult{}, fmt.Errorf("marshal outbox payload: %w", err)
	}
	if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
		IntentID:  intent.IntentID,
		EventType: OutboxEventExecutionIntentCreated,
		Payload:   payload,
	}); err != nil {
		return PollIntakeResult{}, fmt.Errorf("%w: insert outbox: %v", jira.ErrRetryable, err)
	}
	if err := qtx.UpdateJiraWebhookInboxOutcome(ctx, db.UpdateJiraWebhookInboxOutcomeParams{
		EventID:  inbox.EventID,
		IntentID: intent.IntentID,
		Outcome:  PollDispositionAccepted,
	}); err != nil {
		return PollIntakeResult{}, fmt.Errorf("%w: update inbox outcome: %v", jira.ErrRetryable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PollIntakeResult{}, fmt.Errorf("%w: commit: %v", jira.ErrRetryable, err)
	}
	return PollIntakeResult{Disposition: PollDispositionAccepted, IntentID: intentID, IssueKey: issue.Key}, nil
}
