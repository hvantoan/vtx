package application

import (
	"time"

	"github.com/vtx/platform/internal/domain"
)

// ExecutionIntentCreatedV1 is the typed payload persisted to
// outbox_events.payload for event_type execution_intent.created.v1 —
// contract-mirrored in contracts/events/v1/events.json. VTX-3 consumes it.
// `changelog_id` names the Jira changelog.id value; the word "transition_id"
// is deliberately absent (Jira workflow transition IDs are a different,
// VTX-3-owned concept). No credentials, issue description, or raw payload.
type ExecutionIntentCreatedV1 struct {
	EventID        string `json:"event_id"`         // jira_webhook_inbox.event_id
	IntentID       string `json:"intent_id"`        // execution_intents.intent_id
	IssueID        string `json:"issue_id"`         // authoritative Jira issue.id
	IssueKey       string `json:"issue_key"`        // display key (VTX-2)
	JiraSiteID     string `json:"jira_site_id"`     // configured site identity
	ChangelogID    string `json:"changelog_id"`     // Jira changelog.id
	TargetStatusID string `json:"target_status_id"` // changelog `to` status ID
	OccurredAt     string `json:"occurred_at"`      // RFC3339 UTC
	// occurred_at_source records where occurred_at came from:
	// "webhook_timestamp" when the Jira envelope timestamp parsed sanely,
	// "received_at" when it was missing/implausible and intake time is the
	// honest approximation. Never a third value.
	OccurredAtSource string `json:"occurred_at_source,omitempty"`
	CorrelationID    string `json:"correlation_id"`
}

// occurredAtSource values.
const (
	OccurredSourceWebhookTimestamp = "webhook_timestamp"
	OccurredSourceReceivedAt       = "received_at"
)

// OccurredAt derives the event's occurred_at from the Jira webhook envelope
// timestamp (epoch milliseconds per Jira's format; seconds tolerated under
// the >1e12 heuristic), normalized to UTC. A missing, zero, or implausible
// value (outside year 2000–2100) falls back to the request's received_at —
// we stamp the fallback explicitly, never fabricate a third source.
func OccurredAt(env *domain.JiraWebhookEnvelope, receivedAt time.Time) (time.Time, string) {
	ts := env.Timestamp
	if ts != 0 {
		var t time.Time
		// Epoch-ms values exceed 1e12 through the year 3368; below that a
		// plausible timestamp is epoch-seconds.
		if ts > 1e12 {
			t = time.UnixMilli(ts).UTC()
		} else {
			t = time.Unix(ts, 0).UTC()
		}
		if t.Year() >= 2000 && t.Year() <= 2100 {
			return t, OccurredSourceWebhookTimestamp
		}
	}
	return receivedAt.UTC(), OccurredSourceReceivedAt
}
