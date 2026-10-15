package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/persistence/db"
)

type stubStatusChecker struct {
	statusID string
	err      error
	calls    int32
	mu       sync.Mutex
}

func (s *stubStatusChecker) FetchIssueStatus(ctx context.Context, issueID string) (string, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.statusID, s.err
}

func (s *stubStatusChecker) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.calls)
}

func loadFixture(t *testing.T, name string) (*domain.JiraWebhookEnvelope, []byte) {
	t.Helper()
	raw, err := os.ReadFile("../integrations/jira/testdata/" + name)
	require.NoError(t, err)
	var env domain.JiraWebhookEnvelope
	require.NoError(t, json.Unmarshal(raw, &env))
	return &env, raw
}

func testConfig() *config.Config {
	return &config.Config{
		JiraSiteID:             "vtxsolution.atlassian.net",
		JiraProjectAllowlist:   []string{"VTX"},
		JiraReadyToDevStatusID: "10007",
	}
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://vtx_user:vtx_password@localhost:49432/vtx_platform?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping DB-backed service tests", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres unreachable (%v) — skipping DB-backed service tests", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// cleanupKeys removes every row created for the fixture's issue IDs.
func cleanupKeys(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM outbox_events WHERE intent_id IN (SELECT intent_id FROM execution_intents WHERE issue_key LIKE 'VTX-%')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM jira_webhook_inbox WHERE issue_key LIKE 'VTX-%' OR issue_key LIKE 'OTHER-%'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM execution_intents WHERE issue_key LIKE 'VTX-%'`)
	require.NoError(t, err)
}

func TestStatusTransitionToScansAllItems(t *testing.T) {
	env, _ := loadFixture(t, "webhook_valid_transition.json")
	to, ok := env.StatusTransitionTo()
	assert.True(t, ok)
	assert.Equal(t, "10007", to, "status item is second in the changelog — positional extraction would fail")

	env, _ = loadFixture(t, "webhook_no_status_item.json")
	_, ok = env.StatusTransitionTo()
	assert.False(t, ok)
}

func TestEnvelopeGatesReturnIgnoredWithoutDB(t *testing.T) {
	checker := &stubStatusChecker{}
	svc := application.NewJiraWebhookService(testConfig(), nil, nil, checker, slog.Default())

	t.Run("non-status item is ignored scope, not a dedup candidate", func(t *testing.T) {
		env, raw := loadFixture(t, "webhook_no_status_item.json")
		res, err := svc.ProcessWebhookTransition(context.Background(), env, raw, "req-1")
		require.NoError(t, err)
		assert.Equal(t, application.DispositionIgnoredScope, res.Disposition)
	})

	t.Run("missing changelog is ignored", func(t *testing.T) {
		env, raw := loadFixture(t, "webhook_missing_changelog.json")
		res, err := svc.ProcessWebhookTransition(context.Background(), env, raw, "req-2")
		require.NoError(t, err)
		assert.Equal(t, application.DispositionIgnoredScope, res.Disposition)
	})

	t.Run("other event types are ignored", func(t *testing.T) {
		env, raw := loadFixture(t, "webhook_valid_transition.json")
		env.WebhookEvent = "jira:issue_created"
		res, err := svc.ProcessWebhookTransition(context.Background(), env, raw, "req-3")
		require.NoError(t, err)
		assert.Equal(t, application.DispositionIgnoredScope, res.Disposition)
	})

	assert.Equal(t, 0, checker.callCount(), "ignored envelopes must never hit the Jira API")
}

func TestAcceptedThenDuplicate(t *testing.T) {
	pool := testPool(t)
	cleanupKeys(t, pool)
	defer cleanupKeys(t, pool)

	checker := &stubStatusChecker{statusID: "10007"}
	svc := application.NewJiraWebhookService(testConfig(), pool, db.New(pool), checker, slog.Default())

	env, raw := loadFixture(t, "webhook_valid_transition.json")
	res, err := svc.ProcessWebhookTransition(context.Background(), env, raw, "req-a")
	require.NoError(t, err)
	assert.Equal(t, application.DispositionAccepted, res.Disposition)
	require.NotEmpty(t, res.IntentID)

	// Second delivery of the same transition key -> duplicate, same intent.
	res2, err := svc.ProcessWebhookTransition(context.Background(), env, raw, "req-b")
	require.NoError(t, err)
	assert.Equal(t, application.DispositionDuplicate, res2.Disposition)
	assert.Equal(t, res.IntentID, res2.IntentID)

	var inbox, intents, outbox int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM jira_webhook_inbox WHERE changelog_id='20001'`).Scan(&inbox))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM execution_intents WHERE issue_key='VTX-2'`).Scan(&intents))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE event_type=$1`, application.OutboxEventExecutionIntentCreated).Scan(&outbox))
	assert.Equal(t, 1, inbox, "exactly one inbox row for the transition key")
	assert.Equal(t, 1, intents, "exactly one intent for the transition key")
	assert.Equal(t, 1, outbox, "exactly one outbox event")
}

func TestConcurrentDuplicateDelivery(t *testing.T) {
	pool := testPool(t)
	cleanupKeys(t, pool)
	defer cleanupKeys(t, pool)

	checker := &stubStatusChecker{statusID: "10007"}
	svc := application.NewJiraWebhookService(testConfig(), pool, db.New(pool), checker, slog.Default())
	env, raw := loadFixture(t, "webhook_valid_transition.json")

	const n = 8
	results := make([]application.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.ProcessWebhookTransition(context.Background(), env, raw, fmt.Sprintf("req-c%d", i))
		}(i)
	}
	wg.Wait()

	var accepted, dup int
	intentSet := map[string]bool{}
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		intentSet[results[i].IntentID] = true
		switch results[i].Disposition {
		case application.DispositionAccepted:
			accepted++
		case application.DispositionDuplicate:
			dup++
		default:
			t.Fatalf("unexpected disposition %q", results[i].Disposition)
		}
	}
	assert.Equal(t, 1, accepted, "exactly one worker wins the insert race")
	assert.Equal(t, n-1, dup)
	assert.Len(t, intentSet, 1, "all responses must report the same intent_id")

	var intents int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM execution_intents WHERE issue_key='VTX-2'`).Scan(&intents))
	assert.Equal(t, 1, intents, "cross-goroutine dedup: exactly one durable intent")
}

func TestRetryableFailureLeavesNoInboxRow(t *testing.T) {
	pool := testPool(t)
	cleanupKeys(t, pool)
	defer cleanupKeys(t, pool)

	checker := &stubStatusChecker{err: fmt.Errorf("%w: jira api status 429", jira.ErrRetryable)}
	svc := application.NewJiraWebhookService(testConfig(), pool, db.New(pool), checker, slog.Default())
	env, raw := loadFixture(t, "webhook_valid_transition.json")

	_, err := svc.ProcessWebhookTransition(context.Background(), env, raw, "req-r")
	require.Error(t, err)
	assert.True(t, errors.Is(err, jira.ErrRetryable))

	var rows int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM jira_webhook_inbox WHERE changelog_id='20001'`).Scan(&rows))
	assert.Equal(t, 0, rows, "retryable failure must NOT claim the dedup key")
}

func TestStaleAndScopePathsWriteTerminalRows(t *testing.T) {
	pool := testPool(t)
	cleanupKeys(t, pool)
	defer cleanupKeys(t, pool)

	ctx := context.Background()

	// Stale: Jira says the issue already left Ready to Dev.
	checker := &stubStatusChecker{statusID: "10005"}
	svc := application.NewJiraWebhookService(testConfig(), pool, db.New(pool), checker, slog.Default())
	env, raw := loadFixture(t, "webhook_valid_transition.json")
	res, err := svc.ProcessWebhookTransition(ctx, env, raw, "req-s")
	require.NoError(t, err)
	assert.Equal(t, application.DispositionIgnoredStale, res.Disposition)

	var outcome string
	require.NoError(t, pool.QueryRow(ctx, `SELECT outcome FROM jira_webhook_inbox WHERE changelog_id='20001'`).Scan(&outcome))
	assert.Equal(t, "ignored_stale", outcome)

	// Out-of-scope project: inbox-only row, no Jira call needed.
	env2, raw2 := loadFixture(t, "webhook_invalid_project.json")
	res2, err := svc.ProcessWebhookTransition(ctx, env2, raw2, "req-o")
	require.NoError(t, err)
	assert.Equal(t, application.DispositionIgnoredScope, res2.Disposition)

	require.NoError(t, pool.QueryRow(ctx, `SELECT outcome FROM jira_webhook_inbox WHERE changelog_id='20003'`).Scan(&outcome))
	assert.Equal(t, "ignored_scope", outcome)
}
