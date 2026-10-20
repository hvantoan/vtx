//go:build integration

// VTX-3 registry service against a REAL Postgres: concurrency, rollback and
// replay behaviour that a fake cannot prove (TR-02, TR-07, TR-10, TR-17,
// TR-19, TR-20, TR-25, AC-02, AC-08).
package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/persistence/db"
)

const integrationMigrationDir = "../../db/migrations"

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "TEST_DATABASE_URL is not set — registry integration suite skipped")
		os.Exit(0)
	}
	// The schema is owned by db/migrations; make this suite independent of the
	// package execution order (the db suite drops and re-applies migrations).
	if out, err := exec.Command("goose", "-dir", integrationMigrationDir, "postgres", dsn, "up").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "goose up failed: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type registryHarness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  application.RegistryStore
	svc    *application.RegistryService
	now    time.Time
	issued int
}

func newRegistryHarness(t *testing.T) *registryHarness {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	now := time.Date(2026, 10, 19, 6, 30, 0, 0, time.UTC)
	store := persistence.NewPgRegistryStore(pool, db.New(pool))
	return &registryHarness{
		t:     t,
		pool:  pool,
		store: store,
		svc:   application.NewRegistryService(store, "registry-intake", fixedClock{t: now}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
		now:   now,
	}
}

// uniqueIssue returns a fresh numeric Jira issue id (jira_issue_id is VARCHAR(32)).
func (h *registryHarness) uniqueIssue() string {
	h.issued++
	return fmt.Sprintf("9%06d%03d", time.Now().Unix()%1000000, h.issued)
}

// seedIntent inserts the VTX-2 intent row the payload refers to (the FK is real).
func (h *registryHarness) seedIntent(issueKey string) db.ExecutionIntent {
	h.t.Helper()
	intent, err := db.New(h.pool).InsertExecutionIntent(context.Background(), db.InsertExecutionIntentParams{
		Source: "jira_webhook", IssueKey: issueKey, TargetStatusID: "10007",
	})
	if err != nil {
		h.t.Fatalf("seed intent: %v", err)
	}
	return intent
}

func (h *registryHarness) payload(intent db.ExecutionIntent, eventID, issueID, issueKey string) []byte {
	body := map[string]any{
		"event_id":         eventID,
		"intent_id":        uuidString(h.t, h.pool, intent.IntentID),
		"issue_id":         issueID,
		"issue_key":        issueKey,
		"jira_site_id":     "vtxsolution.atlassian.net",
		"changelog_id":     "44444",
		"target_status_id": "10007",
		"correlation_id":   "corr-1",
		"occurred_at":      h.now.Format(time.RFC3339),
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("marshal payload: %v", err)
	}
	return encoded
}

func uuidString(t *testing.T, pool *pgxpool.Pool, id any) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), "SELECT $1::uuid::text", id).Scan(&out); err != nil {
		t.Fatalf("render uuid: %v", err)
	}
	return out
}

func newEventID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}

func (h *registryHarness) one(sql string, args ...any) string {
	h.t.Helper()
	var value string
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		h.t.Fatalf("query %q: %v", sql, err)
	}
	return value
}

func (h *registryHarness) count(sql string, args ...any) int64 {
	h.t.Helper()
	var n int64
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		h.t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func (h *registryHarness) cleanup(siteID, issueID string) {
	ctx := context.Background()
	_, _ = h.pool.Exec(ctx, `DELETE FROM task_runs WHERE task_id IN (SELECT id FROM registry_tasks WHERE jira_site_id=$1 AND jira_issue_id=$2)`, siteID, issueID)
	_, _ = h.pool.Exec(ctx, `DELETE FROM registry_tasks WHERE jira_site_id=$1 AND jira_issue_id=$2`, siteID, issueID)
}

// TestConsumeIntentAdmitsThenReplays: TR-02/TR-19 (1 intent → 1 run queued v0,
// replay ×3 does not duplicate) and AC-02 (commit_sha NULL after intake).
func TestConsumeIntentAdmitsThenReplays(t *testing.T) {
	h := newRegistryHarness(t)
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	intent := h.seedIntent("VTX-" + issueID)
	payload := h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID)

	first, err := h.svc.ConsumeIntent(context.Background(), payload)
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if first.Deferred || first.RunID == "" {
		t.Fatalf("first consume = %+v, want an admitted run", first)
	}

	for i := 2; i <= 3; i++ {
		replay, err := h.svc.ConsumeIntent(context.Background(), payload)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !replay.Duplicate {
			t.Errorf("replay %d: Duplicate = false, want true", i)
		}
	}

	if got := h.count(`SELECT count(*) FROM task_runs WHERE task_id = (SELECT id FROM registry_tasks WHERE jira_issue_id=$1)`, issueID); got != 1 {
		t.Errorf("runs after 3 deliveries = %d, want 1", got)
	}
	outbound := h.count(`SELECT count(*) FROM outbox_events WHERE event_type=$1 AND intent_id=$2`, application.OutboxEventTaskRunCreated, intent.IntentID)
	if outbound != 1 {
		t.Errorf("task_run.created.v1 rows = %d, want 1", outbound)
	}
	if got := h.count(`SELECT count(*) FROM task_run_events WHERE version = 0 AND run_id = (SELECT run_id FROM task_runs WHERE intent_id=$1)`, intent.IntentID); got != 1 {
		t.Errorf("creation audit rows = %d, want exactly 1 at version 0", got)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE intent_id=$1 AND state='queued' AND version=0 AND attempt=0 AND commit_sha IS NULL`, intent.IntentID); got != 1 {
		t.Errorf("admitted run is not queued/v0/attempt0/commit_sha NULL (AC-02)")
	}
	if got := h.count(`SELECT count(*) FROM inbox_consumptions WHERE consumer_name='registry-intake' AND run_id IS NOT NULL`); got < 1 {
		t.Errorf("receipt was not linked to its run")
	}
}

// TestConsumeIntentDifferentEventsForOneIntent: a new source event ID must
// not bypass exactly-once-per-intent after the first delivery was admitted.
func TestConsumeIntentDifferentEventsForOneIntent(t *testing.T) {
	h := newRegistryHarness(t)
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	intent := h.seedIntent("VTX-" + issueID)
	first, err := h.svc.ConsumeIntent(context.Background(), h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if first.Deferred || first.RunID == "" {
		t.Fatalf("first consume = %+v, want an admitted run", first)
	}

	replay, err := h.svc.ConsumeIntent(context.Background(), h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("different-event replay: %v", err)
	}
	if !replay.Duplicate || replay.Deferred || replay.RunID != "" {
		t.Fatalf("different-event replay = %+v, want duplicate with no new effect", replay)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE intent_id=$1`, intent.IntentID); got != 1 {
		t.Errorf("runs for intent = %d, want 1", got)
	}
	if got := h.count(`SELECT count(*) FROM deferred_intents WHERE intent_id=$1`, intent.IntentID); got != 0 {
		t.Errorf("deferred rows for admitted intent = %d, want 0", got)
	}
	if got := h.count(`SELECT count(*) FROM outbox_events WHERE intent_id=$1`, intent.IntentID); got != 1 {
		t.Errorf("outbox rows for admitted intent = %d, want 1", got)
	}
}

// TestConsumeDeferredIntentDifferentEvents: a new source event ID must not
// emit a second deferred row or deferred outbox event either.
func TestConsumeDeferredIntentDifferentEvents(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	active := h.seedIntent("VTX-" + issueID)
	if _, err := h.svc.ConsumeIntent(ctx, h.payload(active, newEventID(t, h.pool), issueID, "VTX-"+issueID)); err != nil {
		t.Fatalf("active consume: %v", err)
	}
	intent := h.seedIntent("VTX-" + issueID)
	first, err := h.svc.ConsumeIntent(ctx, h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("first deferred consume: %v", err)
	}
	if !first.Deferred || first.RunID != "" {
		t.Fatalf("first deferred consume = %+v, want deferred", first)
	}

	replay, err := h.svc.ConsumeIntent(ctx, h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("different-event deferred replay: %v", err)
	}
	if !replay.Duplicate || replay.Deferred || replay.RunID != "" {
		t.Fatalf("different-event deferred replay = %+v, want duplicate with no new effect", replay)
	}
	if got := h.count(`SELECT count(*) FROM deferred_intents WHERE intent_id=$1`, intent.IntentID); got != 1 {
		t.Errorf("deferred rows for intent = %d, want 1", got)
	}
	if got := h.count(`SELECT count(*) FROM outbox_events WHERE event_type=$1 AND intent_id=$2`, application.OutboxEventTaskRunIntentDeferred, intent.IntentID); got != 1 {
		t.Errorf("deferred outbox rows for intent = %d, want 1", got)
	}
}

// TestConsumeIntentDefersWhileARunIsActive: TR-10/I-04 — a second intent for a
// task with an active run lands in the FIFO wait queue and never starts a run.
func TestConsumeIntentDefersWhileARunIsActive(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	first := h.seedIntent("VTX-" + issueID)
	if _, err := h.svc.ConsumeIntent(ctx, h.payload(first, newEventID(t, h.pool), issueID, "VTX-"+issueID)); err != nil {
		t.Fatalf("first consume: %v", err)
	}

	second := h.seedIntent("VTX-" + issueID)
	outcome, err := h.svc.ConsumeIntent(ctx, h.payload(second, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("deferred consume: %v", err)
	}
	if !outcome.Deferred || outcome.RunID != "" {
		t.Fatalf("outcome = %+v, want deferred with no run", outcome)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE task_id=(SELECT id FROM registry_tasks WHERE jira_issue_id=$1)`, issueID); got != 1 {
		t.Errorf("runs = %d, want 1 (deferred intent must not start a run)", got)
	}
	if got := h.count(`SELECT count(*) FROM deferred_intents WHERE intent_id=$1 AND disposition='pending_deferred'`, second.IntentID); got != 1 {
		t.Errorf("deferred_intents rows = %d, want 1", got)
	}
	if got := h.count(`SELECT count(*) FROM outbox_events WHERE event_type=$1 AND intent_id=$2`, application.OutboxEventTaskRunIntentDeferred, second.IntentID); got != 1 {
		t.Errorf("task_run.intent_deferred.v1 rows = %d, want 1", got)
	}
	// The defer payload must carry the task identity (VTX-5 resolves the queue
	// by task, not by the display issue key).
	deferredTaskID := h.one(`SELECT payload->>'task_id' FROM outbox_events WHERE event_type=$1 AND intent_id=$2`, application.OutboxEventTaskRunIntentDeferred, second.IntentID)
	if deferredTaskID == "" || deferredTaskID != outcome.TaskID {
		t.Errorf("deferred payload task_id = %q, want %q", deferredTaskID, outcome.TaskID)
	}

	// FIFO: two deferred intents resolve in (received_at, intent_id) order.
	third := h.seedIntent("VTX-" + issueID)
	if _, err := h.svc.ConsumeIntent(ctx, h.payload(third, newEventID(t, h.pool), issueID, "VTX-"+issueID)); err != nil {
		t.Fatalf("third consume: %v", err)
	}
	rows, err := db.New(h.pool).ListDeferredIntents(ctx, 10)
	if err != nil {
		t.Fatalf("list deferred: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("deferred queue = %d rows, want >= 2", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].ReceivedAt.Time.Before(rows[i-1].ReceivedAt.Time) {
			t.Errorf("FIFO violated at row %d: %s < %s", i, rows[i].ReceivedAt.Time, rows[i-1].ReceivedAt.Time)
		}
	}
}

// TestTransitionRunConcurrentWriters: TR-07 at the data layer — 20 writers with
// the same expected version, over two independent connection pools (two
// "instances"): exactly one commit, nineteen stale_version.
func TestTransitionRunConcurrentWriters(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	intent := h.seedIntent("VTX-" + issueID)
	admitted, err := h.svc.ConsumeIntent(ctx, h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("admit run: %v", err)
	}

	// A second pool is a second set of sessions — the claim is about processes.
	secondPool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(secondPool.Close)
	secondSvc := application.NewRegistryService(
		persistence.NewPgRegistryStore(secondPool, db.New(secondPool)),
		"registry-intake", fixedClock{t: h.now}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	const writers = 20
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		success  int
		stale    int
		otherErr []error
	)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := h.svc
			if i%2 == 1 {
				svc = secondSvc
			}
			// expected_version 0 is only correct for the run as admitted.
			_, err := svc.TransitionRun(ctx, application.TransitionCommand{
				RunID: admitted.RunID, ExpectedVersion: 0,
				Action: domain.ActionCancel, Actor: domain.ActorOperator, Reason: "concurrent test",
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, domain.ErrStaleVersion):
				stale++
			default:
				otherErr = append(otherErr, err)
			}
		}(i)
	}
	wg.Wait()

	if success != 1 {
		t.Errorf("commits = %d, want exactly 1", success)
	}
	if stale != writers-1 {
		t.Errorf("stale_version = %d, want %d", stale, writers-1)
	}
	if len(otherErr) != 0 {
		t.Errorf("unexpected errors: %v", otherErr)
	}
	if got := h.count(`SELECT count(*) FROM task_run_events WHERE run_id=(SELECT run_id FROM task_runs WHERE intent_id=$1)`, intent.IntentID); got != 2 {
		t.Errorf("audit rows = %d, want 2 (creation + one transition)", got)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE intent_id=$1 AND state='cancelled' AND version=1`, intent.IntentID); got != 1 {
		t.Errorf("run did not end cancelled at version 1")
	}
}

// TestTransitionRunRollsBackAtomically: TR-17/TR-25 — a failure after the row
// update (here: the audit insert collides with a pre-existing version) leaves
// no trace of the transition in task_runs, task_run_events or outbox_events.
func TestTransitionRunRollsBackAtomically(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	intent := h.seedIntent("VTX-" + issueID)
	admitted, err := h.svc.ConsumeIntent(ctx, h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("admit run: %v", err)
	}

	// Occupy the audit slot the transition is about to write.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO task_run_events (run_id, previous_state, current_state, action, actor, actor_type, version)
		 VALUES ($1, 'queued', 'queued', 'poison', 'test', 'system', 1)`,
		admitted.RunID); err != nil {
		t.Fatalf("poison audit row: %v", err)
	}
	outboxBefore := h.count(`SELECT count(*) FROM outbox_events WHERE intent_id=$1`, intent.IntentID)

	_, err = h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 0,
		Action: domain.ActionCancel, Actor: domain.ActorOperator, Reason: "must roll back",
	})
	if err == nil {
		t.Fatal("transition succeeded despite the audit collision")
	}

	if got := h.count(`SELECT count(*) FROM task_runs WHERE run_id=$1 AND state='queued' AND version=0`, admitted.RunID); got != 1 {
		t.Errorf("run was mutated by a rolled-back transaction (TR-17)")
	}
	if got := h.count(`SELECT count(*) FROM outbox_events WHERE intent_id=$1`, intent.IntentID); got != outboxBefore {
		t.Errorf("outbox rows = %d, want %d — the outbound event outlived its rollback", got, outboxBefore)
	}
	if got := h.count(`SELECT count(*) FROM task_run_events WHERE run_id=$1 AND action='cancel'`, admitted.RunID); got != 0 {
		t.Errorf("audit rows for the failed transition = %d, want 0", got)
	}
}

// TestTransitionRunOperationalPaths: block → resume round trip through the real
// store, including the resume_state park/restore contract (I-06, AC-12).
func TestTransitionRunOperationalPaths(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	intent := h.seedIntent("VTX-" + issueID)
	admitted, err := h.svc.ConsumeIntent(ctx, h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("admit run: %v", err)
	}

	blocked, err := h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 0, Action: domain.ActionBlock,
		Actor: domain.ActorOperator, Reason: "waiting on operator",
	})
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	if blocked.State != domain.StateBlocked || blocked.ResumeState == nil || *blocked.ResumeState != domain.StateQueued {
		t.Fatalf("blocked run = %+v, want blocked with resume_state queued", blocked)
	}

	resumed, err := h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 1, Action: domain.ActionResume, Actor: domain.ActorOperator,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.State != domain.StateQueued {
		t.Fatalf("resumed run = %s, want queued", resumed.State)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE run_id=$1 AND resume_state IS NULL`, admitted.RunID); got != 1 {
		t.Errorf("resume_state was not cleared after resume")
	}
	if got := h.count(`SELECT count(*) FROM outbox_events WHERE intent_id=$1 AND event_type=$2`, intent.IntentID, application.OutboxEventTaskRunStateChanged); got != 2 {
		t.Errorf("state_changed events = %d, want 2 (block + resume)", got)
	}

	// TR-15: schedule_retry parks the stage and retry_due restores exactly it.
	retryWait, err := h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 2, Action: domain.ActionScheduleRetry, Actor: domain.ActorPolicy,
	})
	if err != nil {
		t.Fatalf("schedule_retry: %v", err)
	}
	if retryWait.State != domain.StateRetryWait || retryWait.RetryDueAt == nil || retryWait.Attempt != 1 {
		t.Fatalf("retry_wait = %+v, want retry_wait with retry_due_at and attempt 1", retryWait)
	}
	if retryWait.ResumeState == nil || *retryWait.ResumeState != domain.StateQueued {
		t.Fatalf("resume_state = %v, want queued", retryWait.ResumeState)
	}

	// retry_due is not an operator command on queued/blocked, so it presents
	// the lease (SQL-injected until VTX-5 allocates them).
	if _, err := h.pool.Exec(ctx,
		`UPDATE task_runs SET lease_owner=$2, lease_epoch=1, lease_expires_at=$3 WHERE run_id=$1`,
		admitted.RunID, "scheduler-1", h.now.Add(time.Hour)); err != nil {
		t.Fatalf("grant lease: %v", err)
	}

	// The retry is not due yet: forcing it is an explicit, gated action, and
	// with the due time in the future the domain refuses it.
	_, err = h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 3, ExpectedLeaseEpoch: 1,
		Action: domain.ActionRetryDue, Actor: domain.ActorPolicy,
	})
	if err == nil {
		t.Errorf("retry_due before the due time was accepted")
	}

	if _, err := h.pool.Exec(ctx, `UPDATE task_runs SET retry_due_at = $2 WHERE run_id = $1`, admitted.RunID, h.now.Add(-time.Second)); err != nil {
		t.Fatalf("advance retry_due_at: %v", err)
	}
	due, err := h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 3, ExpectedLeaseEpoch: 1,
		Action: domain.ActionRetryDue, Actor: domain.ActorPolicy,
	})
	if err != nil {
		t.Fatalf("retry_due: %v", err)
	}
	if due.State != domain.StateQueued {
		t.Errorf("retry_due landed in %s, want the parked queued stage", due.State)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE run_id=$1 AND resume_state IS NULL AND retry_due_at IS NULL`, admitted.RunID); got != 1 {
		t.Errorf("retry bookkeeping was not cleared after retry_due")
	}
}

// TestConcurrentDistinctIntentsForOneTask: TR-19 — two distinct intents for the
// same issue, consumed concurrently from two independent connection pools.
// Exactly one run is admitted; the other is retained in the FIFO queue.
func TestConcurrentDistinctIntentsForOneTask(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	first := h.seedIntent("VTX-" + issueID)
	second := h.seedIntent("VTX-" + issueID)
	payloads := [][]byte{
		h.payload(first, newEventID(t, h.pool), issueID, "VTX-"+issueID),
		h.payload(second, newEventID(t, h.pool), issueID, "VTX-"+issueID),
	}

	secondPool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(secondPool.Close)
	secondSvc := application.NewRegistryService(
		persistence.NewPgRegistryStore(secondPool, db.New(secondPool)),
		"registry-intake", fixedClock{t: h.now}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admit    int
		deferred int
		errs     []error
	)
	for i, payload := range payloads {
		wg.Add(1)
		go func(i int, payload []byte) {
			defer wg.Done()
			svc := h.svc
			if i == 1 {
				svc = secondSvc
			}
			outcome, err := svc.ConsumeIntent(ctx, payload)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case outcome.Deferred:
				deferred++
			default:
				admit++
			}
		}(i, payload)
	}
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("consume errors: %v", errs)
	}
	if admit != 1 || deferred != 1 {
		t.Fatalf("admitted=%d deferred=%d, want 1/1", admit, deferred)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE task_id=(SELECT id FROM registry_tasks WHERE jira_issue_id=$1)`, issueID); got != 1 {
		t.Errorf("runs = %d, want exactly 1 active run", got)
	}
	if got := h.count(`SELECT count(*) FROM deferred_intents WHERE task_id=(SELECT id FROM registry_tasks WHERE jira_issue_id=$1) AND disposition='pending_deferred'`, issueID); got != 1 {
		t.Errorf("deferred rows = %d, want 1 (no lost re-entry)", got)
	}
}

// TestLeaseFencingAndTerminalRules: TR-04/TR-06/TR-09/TR-15/TR-21.
//
// Nothing in phase 1 allocates a lease (VTX-5 owns admission), so the lease is
// injected with SQL — that is the honest way to prove the fencing branch on a
// real row, and it is exactly what the spec's §5 boundary implies.
func TestLeaseFencingAndTerminalRules(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	intent := h.seedIntent("VTX-" + issueID)
	admitted, err := h.svc.ConsumeIntent(ctx, h.payload(intent, newEventID(t, h.pool), issueID, "VTX-"+issueID))
	if err != nil {
		t.Fatalf("admit run: %v", err)
	}

	grantLease := func(epoch int64) {
		t.Helper()
		if _, err := h.pool.Exec(ctx,
			`UPDATE task_runs SET lease_owner=$2, lease_epoch=$3, lease_expires_at=$4 WHERE run_id=$1`,
			admitted.RunID, "scheduler-1", epoch, h.now.Add(time.Hour)); err != nil {
			t.Fatalf("grant lease: %v", err)
		}
	}

	// TR-09/TR-21: a stale epoch is rejected even though the version matches.
	grantLease(2)
	_, err = h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 0, ExpectedLeaseEpoch: 1,
		Action: domain.ActionBegin, Actor: domain.ActorScheduler,
	})
	if !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("stale epoch: got %v, want ErrStaleLease", err)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE run_id=$1 AND version=0 AND state='queued'`, admitted.RunID); got != 1 {
		t.Errorf("a rejected stale-epoch transition mutated the run")
	}

	// An expired lease fails even when the epoch matches.
	if _, err := h.pool.Exec(ctx, `UPDATE task_runs SET lease_expires_at=$2 WHERE run_id=$1`,
		admitted.RunID, h.now.Add(-time.Minute)); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	_, err = h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 0, ExpectedLeaseEpoch: 2,
		Action: domain.ActionBegin, Actor: domain.ActorScheduler,
	})
	if !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("expired lease: got %v, want ErrStaleLease", err)
	}

	// TR-04: a legal begin with the current epoch commits version 1, one audit
	// row and one outbound event.
	grantLease(2)
	begun, err := h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 0, ExpectedLeaseEpoch: 2,
		Action: domain.ActionBegin, Actor: domain.ActorScheduler,
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if begun.State != domain.StatePreparing || begun.Version != 1 {
		t.Fatalf("begin = %s v%d, want preparing v1", begun.State, begun.Version)
	}
	if got := h.count(`SELECT count(*) FROM task_run_events WHERE run_id=$1 AND version=1 AND action='begin'`, admitted.RunID); got != 1 {
		t.Errorf("audit rows at version 1 = %d, want 1", got)
	}
	if got := h.count(`SELECT count(*) FROM outbox_events WHERE intent_id=$1 AND event_type=$2`, intent.IntentID, application.OutboxEventTaskRunStateChanged); got != 1 {
		t.Errorf("state_changed outbox rows = %d, want 1", got)
	}

	// TR-15: cancellation is explicit and terminal.
	cancelled, err := h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 1, ExpectedLeaseEpoch: 2,
		Action: domain.ActionCancel, Actor: domain.ActorOperator, Reason: "operator stop",
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != domain.StateCancelled || cancelled.Version != 2 || cancelled.FinishedAt == nil {
		t.Fatalf("cancel = %+v, want cancelled v2 with finished_at", cancelled)
	}

	// TR-06: a terminal run cannot restart without a new intent.
	_, err = h.svc.TransitionRun(ctx, application.TransitionCommand{
		RunID: admitted.RunID, ExpectedVersion: 2, ExpectedLeaseEpoch: 2,
		Action: domain.ActionBegin, Actor: domain.ActorScheduler,
	})
	if !errors.Is(err, domain.ErrAlreadyTerminal) {
		t.Fatalf("restart of a terminal run: got %v, want ErrAlreadyTerminal", err)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE run_id=$1 AND state='cancelled' AND version=2`, admitted.RunID); got != 1 {
		t.Errorf("terminal run was mutated")
	}
}

// TestDispatchReplayAfterEffectCommittedBeforeAck: TR-20, deterministically.
// The run, audit, receipt and outbound row are committed, but the source event
// is still unpublished — exactly the crash window. Replay must ACK it and
// create nothing a second time (a random SIGKILL may never land here).
func TestDispatchReplayAfterEffectCommittedBeforeAck(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()
	issueID := h.uniqueIssue()
	t.Cleanup(func() { h.cleanup("vtxsolution.atlassian.net", issueID) })

	intent := h.seedIntent("VTX-" + issueID)
	eventID := newEventID(t, h.pool)
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `DELETE FROM outbox_events WHERE intent_id=$1`, intent.IntentID)
		_, _ = h.pool.Exec(context.Background(), `DELETE FROM inbox_consumptions WHERE source_event_id=$1`, eventID)
	})
	payload := h.payload(intent, eventID, issueID, "VTX-"+issueID)

	outcome, err := h.svc.ConsumeIntent(ctx, payload)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}

	queries := db.New(h.pool)
	if _, err := queries.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
		IntentID:  intent.IntentID,
		EventType: application.OutboxEventExecutionIntentCreated,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("seed source event: %v", err)
	}

	dispatcher := application.NewOutboxDispatcher(
		persistence.NewPgOutboxStore(queries),
		"registry-intake",
		map[string]application.EventHandler{
			application.OutboxEventExecutionIntentCreated: func(ctx context.Context, payload []byte) error {
				_, err := h.svc.ConsumeIntent(ctx, payload)
				return err
			},
		},
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		application.OutboxDispatcherConfig{},
	)
	stats, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("replay dispatch: %v", err)
	}
	if stats.Published != 1 {
		t.Errorf("published = %d, want 1 (the replay must ACK the source event)", stats.Published)
	}
	if got := h.count(`SELECT count(*) FROM task_runs WHERE run_id=$1`, outcome.RunID); got != 1 {
		t.Errorf("runs = %d, want 1", got)
	}
	if got := h.count(`SELECT count(*) FROM task_run_events WHERE run_id=$1 AND version=0`, outcome.RunID); got != 1 {
		t.Errorf("creation audits = %d, want 1", got)
	}
	if got := h.count(`SELECT count(*) FROM outbox_events WHERE intent_id=$1 AND event_type=$2`,
		intent.IntentID, application.OutboxEventTaskRunCreated); got != 1 {
		t.Errorf("task_run.created.v1 rows = %d, want 1 (no second effect)", got)
	}
	if got := h.count(`SELECT count(*) FROM inbox_consumptions WHERE consumer_name='registry-intake' AND source_event_id=$1`, eventID); got != 1 {
		t.Errorf("receipts = %d, want 1", got)
	}
}
