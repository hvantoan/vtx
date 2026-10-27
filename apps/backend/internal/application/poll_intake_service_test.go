package application_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/persistence/db"
)

type pollFixedClock struct{ at time.Time }

func (c pollFixedClock) Now() time.Time { return c.at }

func pollService(t *testing.T, pool *pgxpool.Pool, fetcher jira.IssueFetcher) *application.PollIntakeService {
	t.Helper()
	return application.NewPollIntakeService(
		testConfig(), pool, db.New(pool), fetcher,
		pollFixedClock{at: testReceivedAt}, slog.Default(),
	)
}

func TestPollIntakeAcceptedThenDuplicate(t *testing.T) {
	pool := testPool(t)
	cleanupKeys(t, pool)
	defer cleanupKeys(t, pool)

	fetcher := &stubIssueFetcher{issue: jira.FetchedIssue{Key: "VTX-2", ProjectKey: "VTX", StatusID: "10007"}}
	svc := pollService(t, pool, fetcher)
	req := application.PollIntakeRequest{IssueID: "10014", IssueKey: "VTX-2", Version: "2026-10-27 10:00:00 +07"}

	res, err := svc.Intake(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, application.PollDispositionAccepted, res.Disposition)
	require.NotEmpty(t, res.IntentID)

	// Same version re-delivered (at-least-once queue) must reuse the intent.
	res2, err := svc.Intake(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, application.PollDispositionDuplicate, res2.Disposition)
	assert.Equal(t, res.IntentID, res2.IntentID)

	var intents, outbox int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM execution_intents WHERE issue_key='VTX-2' AND source='jira_poll'`).Scan(&intents))
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE event_type=$1 AND intent_id=$2::uuid`,
		application.OutboxEventExecutionIntentCreated, res.IntentID).Scan(&outbox))
	assert.Equal(t, 1, intents, "one durable intent per issue version")
	assert.Equal(t, 1, outbox, "one outbox event per intent")
}

func TestPollIntakeNewVersionCreatesNewIntent(t *testing.T) {
	pool := testPool(t)
	cleanupKeys(t, pool)
	defer cleanupKeys(t, pool)

	fetcher := &stubIssueFetcher{issue: jira.FetchedIssue{Key: "VTX-2", ProjectKey: "VTX", StatusID: "10007"}}
	svc := pollService(t, pool, fetcher)

	first, err := svc.Intake(context.Background(), application.PollIntakeRequest{IssueID: "10014", IssueKey: "VTX-2", Version: "v1"})
	require.NoError(t, err)
	second, err := svc.Intake(context.Background(), application.PollIntakeRequest{IssueID: "10014", IssueKey: "VTX-2", Version: "v2"})
	require.NoError(t, err)
	assert.Equal(t, application.PollDispositionAccepted, second.Disposition)
	assert.NotEqual(t, first.IntentID, second.IntentID)
}

func TestPollIntakeStatusDriftWritesNothing(t *testing.T) {
	pool := testPool(t)
	cleanupKeys(t, pool)
	defer cleanupKeys(t, pool)

	fetcher := &stubIssueFetcher{issue: jira.FetchedIssue{Key: "VTX-2", ProjectKey: "VTX", StatusID: "10008"}}
	svc := pollService(t, pool, fetcher)

	res, err := svc.Intake(context.Background(), application.PollIntakeRequest{IssueID: "10014", IssueKey: "VTX-2", Version: "v1"})
	require.NoError(t, err)
	assert.Equal(t, application.PollDispositionIgnoredStale, res.Disposition)
	assert.Equal(t, application.PollReasonStatusDrift, res.ReasonCode)

	var intents int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM execution_intents WHERE issue_key='VTX-2'`).Scan(&intents))
	assert.Zero(t, intents)
}

func TestPollIntakeRejectsUnconfiguredProjectBeforeJira(t *testing.T) {
	pool := testPool(t)
	fetcher := &stubIssueFetcher{}
	svc := pollService(t, pool, fetcher)

	res, err := svc.Intake(context.Background(), application.PollIntakeRequest{IssueID: "10014", IssueKey: "OTHER-2", Version: "v1"})
	require.NoError(t, err)
	assert.Equal(t, application.PollDispositionIgnoredScope, res.Disposition)
	assert.Equal(t, application.PollReasonProjectNotAllowed, res.ReasonCode)
	assert.Zero(t, fetcher.callCount(), "must not call Jira for an unconfigured project")
}

func TestPollIntakeIdentityMismatch(t *testing.T) {
	pool := testPool(t)
	fetcher := &stubIssueFetcher{issue: jira.FetchedIssue{Key: "OTHER-2", ProjectKey: "OTHER", StatusID: "10007"}}
	svc := pollService(t, pool, fetcher)

	res, err := svc.Intake(context.Background(), application.PollIntakeRequest{IssueID: "10014", IssueKey: "VTX-2", Version: "v1"})
	require.NoError(t, err)
	assert.Equal(t, application.PollDispositionIgnoredScope, res.Disposition)
	assert.Equal(t, application.PollReasonIdentityMismatch, res.ReasonCode)
}

func TestPollIntakeValidatesInput(t *testing.T) {
	pool := testPool(t)
	svc := pollService(t, pool, &stubIssueFetcher{})

	_, err := svc.Intake(context.Background(), application.PollIntakeRequest{IssueID: "not-numeric", IssueKey: "VTX-2", Version: "v1"})
	assert.ErrorIs(t, err, application.ErrPayloadInvalid)

	_, err = svc.Intake(context.Background(), application.PollIntakeRequest{IssueID: "10014", IssueKey: "VTX-2"})
	assert.ErrorIs(t, err, application.ErrPayloadInvalid)
}
