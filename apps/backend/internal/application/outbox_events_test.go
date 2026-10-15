package application_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
)

func TestOccurredAt(t *testing.T) {
	receivedAt := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		ts         int64
		wantSource string
		wantTime   time.Time
	}{
		{"epoch milliseconds", 1760500000000, application.OccurredSourceWebhookTimestamp, time.UnixMilli(1760500000000).UTC()},
		{"epoch seconds", 1760500000, application.OccurredSourceWebhookTimestamp, time.Unix(1760500000, 0).UTC()},
		{"zero falls back", 0, application.OccurredSourceReceivedAt, receivedAt},
		{"pre-2000 garbage falls back", 1000, application.OccurredSourceReceivedAt, receivedAt},
		{"post-2100 garbage falls back", 99999999999999, application.OccurredSourceReceivedAt, receivedAt},
		{"negative falls back", -5, application.OccurredSourceReceivedAt, receivedAt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &domain.JiraWebhookEnvelope{Timestamp: tc.ts}
			got, src := application.OccurredAt(env, receivedAt)
			assert.Equal(t, tc.wantSource, src)
			assert.Equal(t, tc.wantTime, got)
			assert.Equal(t, time.UTC, got.Location(), "occurred_at is always UTC")
		})
	}
}

func TestExecutionIntentCreatedV1ContractShape(t *testing.T) {
	payload, err := json.Marshal(application.ExecutionIntentCreatedV1{
		EventID:          "11111111-1111-1111-1111-111111111111",
		IntentID:         "22222222-2222-2222-2222-222222222222",
		IssueID:          "10011",
		IssueKey:         "VTX-2",
		JiraSiteID:       "vtxsolution.atlassian.net",
		ChangelogID:      "20001",
		TargetStatusID:   "10007",
		OccurredAt:       time.UnixMilli(1760500000000).UTC().Format(time.RFC3339),
		OccurredAtSource: application.OccurredSourceWebhookTimestamp,
		CorrelationID:    "req-x",
	})
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(payload, &m))

	for _, key := range []string{
		"event_id", "intent_id", "issue_id", "issue_key", "jira_site_id",
		"changelog_id", "target_status_id", "occurred_at", "correlation_id",
		"occurred_at_source",
	} {
		assert.Contains(t, m, key, "contract requires %q", key)
	}
	assert.Equal(t, "webhook_timestamp", m["occurred_at_source"])
	assert.NotContains(t, m, "transition_id",
		"changelog_id replaces transition_id — VTX-3 workflow transition IDs are a different concept")
}
