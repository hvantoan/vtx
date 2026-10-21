//go:build integration

// VTX-3 schema invariants against a REAL Postgres (TR-11, TR-18, TR-24 and
// I-01/I-02/I-03/I-05). The suite owns the database it is pointed at: it runs
// goose up/down/up and seeds VTX-2 rows. Point TEST_DATABASE_URL at a throwaway
// database (CI: the compose postgres of the e2e stack).
package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationDir = "../../../db/migrations"

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "TEST_DATABASE_URL is not set — integration suite skipped")
		os.Exit(0)
	}
	// Every test assumes the schema is present; the lifecycle test then owns
	// the up/down/up proof on top of it.
	if out, err := exec.Command("goose", "-dir", migrationDir, "postgres", dsn, "up").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "goose up failed: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is not set")
	}
	return dsn
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func goose(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("goose", append([]string{"-dir", migrationDir, "postgres", testDSN(t)}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("goose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	t.Logf("goose %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
}

// mustSQLState asserts the Postgres error class, not just "an error happened".
func mustSQLState(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got nil error", want)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a Postgres error (%s), got %T: %v", want, err, err)
	}
	if pgErr.Code != want {
		t.Fatalf("expected SQLSTATE %s, got %s: %v", want, pgErr.Code, err)
	}
}

func scalarInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func scalarString(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return s
}

// seedIntent inserts the minimal VTX-2 rows a run depends on.
func seedIntent(t *testing.T, q *Queries, issueKey string) pgtype.UUID {
	t.Helper()
	intent, err := q.InsertExecutionIntent(context.Background(), InsertExecutionIntentParams{
		Source:         "jira_webhook",
		IssueKey:       issueKey,
		TargetStatusID: "10007",
	})
	if err != nil {
		t.Fatalf("insert execution_intent: %v", err)
	}
	return intent.IntentID
}

func seedTask(t *testing.T, q *Queries, siteID, issueID string) RegistryTask {
	t.Helper()
	task, err := q.UpsertRegistryTask(context.Background(), UpsertRegistryTaskParams{
		JiraSiteID:   siteID,
		JiraIssueID:  issueID,
		JiraIssueKey: "VTX-" + issueID,
		ProjectKey:   "VTX",
	})
	if err != nil {
		t.Fatalf("upsert registry_task: %v", err)
	}
	return task
}

// uniqueIssueID models a real Jira issue id: numeric and short, because
// jira_issue_id is VARCHAR(32) to stay identical to VTX-2's inbox column.
func uniqueIssueID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), "SELECT (extract(epoch from clock_timestamp())*1000000)::bigint::text").Scan(&id); err != nil {
		t.Fatalf("issue id: %v", err)
	}
	return id
}

// TestMigrationLifecycle: TR-11 (up/down/up), TR-18/TR-24 (rows, ids and
// pending outbox survive the additive migration).
func TestMigrationLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	q := New(pool)

	goose(t, "down-to", "0")
	goose(t, "up-to", "3")

	intent := seedIntent(t, q, "VTX-TR18")
	outbox, err := q.InsertOutboxEvent(ctx, InsertOutboxEventParams{
		IntentID:  intent,
		EventType: "execution_intent.created.v1",
		Payload:   []byte(`{"event_id":"00000000-0000-0000-0000-000000000001"}`),
	})
	if err != nil {
		t.Fatalf("insert outbox event: %v", err)
	}

	before := map[string]int64{}
	for _, table := range []string{"tasks", "task_events", "agent_sessions", "execution_intents", "outbox_events", "jira_webhook_inbox", "webhook_receipts"} {
		before[table] = scalarInt(t, pool, "SELECT count(*) FROM "+table)
	}
	beforePending := scalarInt(t, pool, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL")

	goose(t, "up") // 00004 — the additive registry migration

	for table, want := range before {
		if got := scalarInt(t, pool, "SELECT count(*) FROM "+table); got != want {
			t.Errorf("TR-18: %s rows after migration = %d, want %d (migration must not touch VTX-2/bootstrap data)", table, got, want)
		}
	}
	if got := scalarInt(t, pool, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL"); got != beforePending {
		t.Errorf("TR-24: pending outbox after migration = %d, want %d", got, beforePending)
	}
	if got := scalarInt(t, pool, "SELECT count(*) FROM outbox_events WHERE outbox_id = $1", outbox.OutboxID); got != 1 {
		t.Errorf("TR-24: pre-existing outbox row %v disappeared", outbox.OutboxID)
	}
	if got := scalarInt(t, pool, "SELECT count(*) FROM execution_intents WHERE intent_id = $1", intent); got != 1 {
		t.Errorf("TR-18: pre-existing intent %v disappeared", intent)
	}

	// The migration must have added the registry objects, by name.
	for _, table := range []string{"registry_tasks", "task_runs", "task_run_events", "deferred_intents", "inbox_consumptions"} {
		if got := scalarInt(t, pool, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1", table); got != 1 {
			t.Errorf("missing table %s after migration", table)
		}
	}
	for _, index := range []string{"registry_tasks_identity_unique", "task_runs_intent_id_key", "task_run_events_version_unique", "idx_task_runs_active_per_task", "idx_deferred_intents_fifo"} {
		if got := scalarInt(t, pool, "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname=$1", index); got != 1 {
			t.Errorf("missing index/constraint %s after migration", index)
		}
	}
	if def := scalarString(t, pool, "SELECT indexdef FROM pg_indexes WHERE indexname='idx_task_runs_active_per_task'"); !strings.Contains(def, "<> ALL") || !strings.Contains(def, "'completed'") {
		t.Errorf("I-03: partial unique index lost its terminal-state predicate: %s", def)
	}
	for _, fk := range []string{"task_runs_task_id_fkey", "task_runs_intent_id_fkey", "task_run_events_run_id_fkey", "deferred_intents_intent_id_fkey", "inbox_consumptions_run_id_fkey"} {
		if got := scalarInt(t, pool, "SELECT count(*) FROM pg_constraint WHERE conname=$1", fk); got != 1 {
			t.Errorf("missing foreign key %s after migration", fk)
		}
	}

	// TR-11: both directions, then back to head for the remaining tests.
	goose(t, "down-to", "0")
	goose(t, "up")
	if got := scalarInt(t, pool, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='task_runs'"); got != 1 {
		t.Fatalf("TR-11: re-up did not recreate task_runs")
	}
}

// TestI01TaskIdentityUnique: identity is (jira_site_id, jira_issue_id); the
// issue key is mutable metadata, not part of the key.
func TestI01TaskIdentityUnique(t *testing.T) {
	pool := testPool(t)
	q := New(pool)
	issueID := uniqueIssueID(t, pool)
	site := "vtxsolution.atlassian.net"

	first := seedTask(t, q, site, issueID)
	t.Cleanup(func() { pool.Exec(context.Background(), "DELETE FROM registry_tasks WHERE id = $1", first.ID) })

	second, err := q.UpsertRegistryTask(context.Background(), UpsertRegistryTaskParams{
		JiraSiteID:   site,
		JiraIssueID:  issueID,
		JiraIssueKey: "VTX-RENAMED",
		ProjectKey:   "VTX2",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("I-01: upsert created a second task (%v != %v)", second.ID, first.ID)
	}
	if second.JiraIssueKey != "VTX-RENAMED" {
		t.Errorf("I-01: issue key is metadata and must be updatable, got %q", second.JiraIssueKey)
	}
	if got := scalarInt(t, pool, "SELECT count(*) FROM registry_tasks WHERE jira_site_id=$1 AND jira_issue_id=$2", site, issueID); got != 1 {
		t.Errorf("I-01: %d rows for one identity, want 1", got)
	}

	_, err = pool.Exec(context.Background(),
		`INSERT INTO registry_tasks (jira_site_id, jira_issue_id, jira_issue_key, project_key) VALUES ($1,$2,$3,$4)`,
		site, issueID, "VTX-DUPLICATE", "VTX")
	mustSQLState(t, err, "23505")
}

// TestI02RunReceiptAuditUniqueness: one run per intent, one receipt per
// (consumer, event), one audit row per (run, version).
func TestI02RunReceiptAuditUniqueness(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	q := New(pool)
	task := seedTask(t, q, "vtxsolution.atlassian.net", uniqueIssueID(t, pool))
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM registry_tasks WHERE id = $1", task.ID) })

	intent := seedIntent(t, q, "VTX-I02")
	run, err := q.InsertTaskRun(ctx, InsertTaskRunParams{TaskID: task.ID, IntentID: intent})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO task_runs (task_id, intent_id, state) VALUES ($1,$2,'queued')`,
		task.ID, intent)
	mustSQLState(t, err, "23505")

	event := InsertTaskRunEventParams{
		RunID:         run.RunID,
		PreviousState: "queued",
		CurrentState:  "queued",
		Action:        "created",
		Actor:         "registry",
		ActorType:     "system",
		Version:       0,
	}
	if _, err := q.InsertTaskRunEvent(ctx, event); err != nil {
		t.Fatalf("insert audit v0: %v", err)
	}
	_, err = q.InsertTaskRunEvent(ctx, event)
	mustSQLState(t, err, "23505")

	consumption := InsertInboxConsumptionParams{
		ConsumerName:  "registry-intake",
		SourceEventID: run.RunID, // any uuid works as the source event id here
		RunID:         run.RunID,
	}
	if _, err := q.InsertInboxConsumption(ctx, consumption); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	if _, err := q.InsertInboxConsumption(ctx, consumption); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("I-02: duplicate receipt must be a no-op (pgx.ErrNoRows), got %v", err)
	}
	if got := scalarInt(t, pool, "SELECT count(*) FROM inbox_consumptions WHERE consumer_name=$1 AND source_event_id=$2", consumption.ConsumerName, consumption.SourceEventID); got != 1 {
		t.Errorf("I-02: %d receipts for one (consumer, event), want 1", got)
	}
}

// TestI03OneActiveRunPerTask: the partial unique index, proven across TWO
// database sessions (not two goroutines sharing one connection).
func TestI03OneActiveRunPerTask(t *testing.T) {
	ctx := context.Background()
	poolA := testPool(t)
	poolB := testPool(t)
	qA, qB := New(poolA), New(poolB)

	task := seedTask(t, qA, "vtxsolution.atlassian.net", uniqueIssueID(t, poolA))
	t.Cleanup(func() { poolA.Exec(ctx, "DELETE FROM registry_tasks WHERE id = $1", task.ID) })

	run1, err := qA.InsertTaskRun(ctx, InsertTaskRunParams{TaskID: task.ID, IntentID: seedIntent(t, qA, "VTX-I03-A")})
	if err != nil {
		t.Fatalf("first run (session A): %v", err)
	}
	_, err = qB.InsertTaskRun(ctx, InsertTaskRunParams{TaskID: task.ID, IntentID: seedIntent(t, qB, "VTX-I03-B")})
	mustSQLState(t, err, "23505")

	// A terminal run frees the slot (I-06 + I-03).
	if _, err := qA.UpdateTaskRunState(ctx, UpdateTaskRunStateParams{RunID: run1.RunID, State: "completed", Version: 0}); err != nil {
		t.Fatalf("complete run1: %v", err)
	}
	if _, err := qB.InsertTaskRun(ctx, InsertTaskRunParams{TaskID: task.ID, IntentID: seedIntent(t, qB, "VTX-I03-C")}); err != nil {
		t.Fatalf("I-03: after the first run reached a terminal state the next run must be admitted: %v", err)
	}
}

// TestI05NewRunDefaultsAndCAS: a new run starts at state queued/version 0 with
// an audit row at version 0; the CAS update advances exactly one version and a
// stale CAS affects zero rows.
func TestI05NewRunDefaultsAndCAS(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	q := New(pool)
	task := seedTask(t, q, "vtxsolution.atlassian.net", uniqueIssueID(t, pool))
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM registry_tasks WHERE id = $1", task.ID) })

	run, err := q.InsertTaskRun(ctx, InsertTaskRunParams{TaskID: task.ID, IntentID: seedIntent(t, q, "VTX-I05")})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if run.State != "queued" || run.Version != 0 || run.Attempt != 0 {
		t.Errorf("I-05: new run = state %q version %d attempt %d, want queued/0/0", run.State, run.Version, run.Attempt)
	}
	if run.CommitSha.Valid {
		t.Errorf("I-05: commit_sha must be NULL for a queued run")
	}
	if _, err := q.InsertTaskRunEvent(ctx, InsertTaskRunEventParams{
		RunID: run.RunID, PreviousState: "queued", CurrentState: "queued",
		Action: "created", Actor: "registry", ActorType: "system", Version: 0,
	}); err != nil {
		t.Fatalf("I-05: creation audit at version 0: %v", err)
	}

	updated, err := q.UpdateTaskRunState(ctx, UpdateTaskRunStateParams{
		RunID: run.RunID, State: "preparing", Version: 0,
		StartedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		t.Fatalf("first CAS: %v", err)
	}
	if updated.Version != 1 {
		t.Errorf("I-05: version after one transition = %d, want 1", updated.Version)
	}

	if _, err := q.UpdateTaskRunState(ctx, UpdateTaskRunStateParams{RunID: run.RunID, State: "coding", Version: 0}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("I-05: stale CAS must affect 0 rows (pgx.ErrNoRows), got %v", err)
	}

	// No cursor: the version-0 creation audit must be included.
	events, err := q.ListTaskRunEvents(ctx, ListTaskRunEventsParams{RunID: run.RunID, RowLimit: 10})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 || events[0].Version != 0 {
		t.Errorf("I-05: audit rows = %d, want exactly the version-0 creation row", len(events))
	}
}
