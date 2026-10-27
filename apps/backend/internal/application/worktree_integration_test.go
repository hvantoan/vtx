//go:build integration

// VTX-4 worktree lifecycle against REAL Postgres + REAL git: concurrent
// allocation, same-run idempotency, task-busy exclusion, epoch fencing,
// terminal-run rejection, and crash-window reconcile. The schema is applied
// by the suite's TestMain (shared with the registry integration suite).
package application_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/gitops"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/persistence/db"
)

// worktreeHarness seeds VTX-3 rows directly (task + run) and wires the real
// store with a real git runner rooted in temp dirs.
type worktreeHarness struct {
	t       *testing.T
	pool    *pgxpool.Pool
	svc     *application.WorktreeService
	store   *persistence.PgWorktreeStore
	srcRepo string
	layout  gitops.Layout
	seq     int
}

func newWorktreeHarness(t *testing.T) *worktreeHarness {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// A real local repo as the clone source.
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	base := t.TempDir()
	layout := gitops.Layout{
		WorktreeRoot:   filepath.Join(base, "wt"),
		QuarantineRoot: filepath.Join(base, "q"),
		RepoCacheRoot:  filepath.Join(base, "cache"),
	}
	if err := os.MkdirAll(layout.WorktreeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	store := persistence.NewPgWorktreeStore(pool, db.New(pool))
	h := &worktreeHarness{
		t: t, pool: pool, srcRepo: src, layout: layout, store: store,
		svc: application.NewWorktreeService(
			store,
			gitops.Runner{Git: gitops.Git{}, Layout: layout},
			"test-worker", time.Minute,
			application.SystemClock{},
			slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		),
	}
	return h
}

func (h *worktreeHarness) seedTaskAndRun(state string) (taskID, runID string) {
	h.t.Helper()
	h.seq++
	key := fmt.Sprintf("VTX-%d-%d", time.Now().UnixNano()%1_000_000, h.seq)
	err := h.pool.QueryRow(context.Background(),
		`INSERT INTO registry_tasks (jira_site_id, jira_issue_id, jira_issue_key, project_key, repo_url)
		 VALUES ('site', $1, $2, 'VTX', $3) RETURNING id`,
		"issue-"+key, key, h.srcRepo,
	).Scan(&taskID)
	if err != nil {
		h.t.Fatalf("seed task: %v", err)
	}
	err = h.pool.QueryRow(context.Background(),
		`INSERT INTO execution_intents (source, issue_key, target_status_id)
		 VALUES ('test', $1, 's') RETURNING intent_id`, key,
	).Scan(&runID)
	if err != nil {
		h.t.Fatalf("seed intent: %v", err)
	}
	err = h.pool.QueryRow(context.Background(),
		`INSERT INTO task_runs (task_id, intent_id, state) VALUES ($1, $2, $3) RETURNING run_id`,
		taskID, runID, state,
	).Scan(&runID)
	if err != nil {
		h.t.Fatalf("seed run: %v", err)
	}
	return taskID, runID
}

func (h *worktreeHarness) worktreeCount(runID string) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM worktrees WHERE run_id = $1`, runID).Scan(&n); err != nil {
		h.t.Fatalf("count: %v", err)
	}
	return n
}

func TestWorktreeAllocate_ConcurrentSameRun(t *testing.T) {
	h := newWorktreeHarness(t)
	_, runID := h.seedTaskAndRun("preparing")

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("allocator %d: %v", i, err)
		}
	}
	if got := h.worktreeCount(runID); got != 1 {
		t.Fatalf("worktree rows = %d, want 1", got)
	}
}

func TestWorktreeAllocate_IdempotentRetry(t *testing.T) {
	h := newWorktreeHarness(t)
	_, runID := h.seedTaskAndRun("preparing")
	first, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	second, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !second.Existing || second.Worktree.WorktreeID != first.Worktree.WorktreeID {
		t.Fatal("retry did not return the same row")
	}
	if _, err := os.Stat(filepath.Join(first.Worktree.Path, ".git")); err != nil {
		t.Fatalf("worktree missing on disk: %v", err)
	}
	// Lease was written by the allocation (VTX-3 columns, VTX-4 writer).
	var owner *string
	var epoch int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT lease_owner, lease_epoch FROM task_runs WHERE run_id = $1`, runID,
	).Scan(&owner, &epoch); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if owner == nil || *owner != "test-worker" || epoch != first.LeaseEpoch {
		t.Fatalf("lease = %v/%d, want test-worker/%d", owner, epoch, first.LeaseEpoch)
	}
	// Branch written to the VTX-3 column.
	var branch *string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT branch FROM task_runs WHERE run_id = $1`, runID).Scan(&branch); err != nil {
		t.Fatalf("read branch: %v", err)
	}
	if branch == nil || *branch != first.Worktree.Branch {
		t.Fatalf("run branch = %v", branch)
	}
}

func TestWorktreeAllocate_ReacquiresLeaseAfterResume(t *testing.T) {
	h := newWorktreeHarness(t)
	_, runID := h.seedTaskAndRun("preparing")
	first, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// The block path releases the lease; an operator resume then sends the run
	// back to preparing with the same worktree row.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE task_runs SET state = 'blocked', error_code = 'orchestrator_stalled',
		 lease_owner = NULL, lease_expires_at = NULL WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("simulate block: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE task_runs SET state = 'preparing' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("simulate resume: %v", err)
	}
	second, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID})
	if err != nil {
		t.Fatalf("reallocate: %v", err)
	}
	if !second.Existing {
		t.Fatal("reallocation must return the existing row")
	}
	if second.LeaseEpoch <= first.LeaseEpoch {
		t.Fatalf("lease epoch = %d, want > %d (a resumed run must be re-fenced)", second.LeaseEpoch, first.LeaseEpoch)
	}
	var owner *string
	var expires *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT lease_owner, lease_expires_at FROM task_runs WHERE run_id = $1`, runID,
	).Scan(&owner, &expires); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if owner == nil || *owner != "test-worker" || expires == nil || !expires.After(time.Now()) {
		t.Fatalf("lease not re-acquired: owner=%v expires=%v", owner, expires)
	}
}

func TestWorktree_OneActivePerTaskEnforced(t *testing.T) {
	h := newWorktreeHarness(t)
	taskID, runID := h.seedTaskAndRun("preparing")
	if _, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID}); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// The service path can never reach this (I-03 caps active runs per task
	// at one), so prove the DB guard directly: a second allocated row on the
	// same task must violate the partial unique index with SQLSTATE 23505.
	_, runID2 := h.seedTaskAndRun("preparing")
	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO worktrees (run_id, task_id, status, path, branch, repo_url)
		 VALUES ($1, $2, 'allocated', '/tmp/x', 'b', 'r')`, runID2, taskID)
	if err == nil {
		t.Fatal("second allocated worktree on same task was accepted")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("err = %v, want SQLSTATE 23505", err)
	}
}

func TestWorktreeAllocate_TerminalRunRejected(t *testing.T) {
	h := newWorktreeHarness(t)
	for _, state := range []string{"completed", "failed", "cancelled"} {
		_, runID := h.seedTaskAndRun(state)
		_, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID})
		if !errors.Is(err, application.ErrWorktreeRunTerminal) {
			t.Fatalf("state %s: err = %v, want ErrWorktreeRunTerminal", state, err)
		}
	}
}

func TestWorktreeRelease_DirtyQuarantinedCleanRemoved(t *testing.T) {
	h := newWorktreeHarness(t)
	_, runClean := h.seedTaskAndRun("preparing")
	_, runDirty := h.seedTaskAndRun("preparing")

	clean, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runClean})
	if err != nil {
		t.Fatalf("allocate clean: %v", err)
	}
	dirty, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runDirty})
	if err != nil {
		t.Fatalf("allocate dirty: %v", err)
	}

	// Clean → released, dir gone.
	wt, err := h.svc.ReleaseWorktree(context.Background(), runClean, "done")
	if err != nil {
		t.Fatalf("release clean: %v", err)
	}
	if wt.Status != application.WorktreeReleased {
		t.Fatalf("clean status = %s", wt.Status)
	}
	if _, err := os.Stat(clean.Worktree.Path); !os.IsNotExist(err) {
		t.Fatalf("clean worktree still on disk: %v", err)
	}

	// Dirty (untracked file) → quarantined, dir moved not deleted.
	if err := os.WriteFile(filepath.Join(dirty.Worktree.Path, "junk.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err = h.svc.ReleaseWorktree(context.Background(), runDirty, "done")
	if err != nil {
		t.Fatalf("release dirty: %v", err)
	}
	if wt.Status != application.WorktreeQuarantined {
		t.Fatalf("dirty status = %s", wt.Status)
	}
	if _, err := os.Stat(filepath.Join(h.layout.QuarantinePath(runDirty), "junk.txt")); err != nil {
		t.Fatalf("quarantined file missing: %v", err)
	}
}

func TestWorktreeReconcile_TerminalRunAndOrphan(t *testing.T) {
	h := newWorktreeHarness(t)
	_, runID := h.seedTaskAndRun("preparing")
	alloc, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// Terminal runs are always reclaimable. Expired leases on NON-terminal
	// runs are not reclaimed until VTX-7 arms ReclaimExpiredLeases — with no
	// renewal caller every live lease expires one TTL after allocation.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE task_runs SET state = 'failed' WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	// Crash-window orphan: dir that looks like an allocation but has no row.
	orphan := h.layout.WorktreePath("deadbeef-0000-0000-0000-000000000000")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	rep, err := h.svc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Released != 1 {
		t.Fatalf("released = %d, errors=%v", rep.Released, rep.Errors)
	}
	if rep.OrphansQuarantined != 1 {
		t.Fatalf("orphans = %d, errors=%v", rep.OrphansQuarantined, rep.Errors)
	}
	if _, err := os.Stat(alloc.Worktree.Path); !os.IsNotExist(err) {
		t.Fatal("terminal run's worktree still present")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan dir still present")
	}
	// Cleanup must also clear the run's lease row.
	var owner *string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT lease_owner FROM task_runs WHERE run_id = $1`, runID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		t.Fatalf("lease owner still set after reconcile cleanup: %v", *owner)
	}
}

func TestWorktreeAllocate_LeaseExpirySet(t *testing.T) {
	h := newWorktreeHarness(t)
	_, runID := h.seedTaskAndRun("preparing")
	alloc, err := h.svc.AllocateWorktree(context.Background(), application.AllocateCommand{RunID: runID})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	var expires time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM task_runs WHERE run_id = $1`, runID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires.Before(time.Now()) {
		t.Fatalf("lease already expired: %v", expires)
	}
	if alloc.LeaseEpoch <= 0 {
		t.Fatalf("epoch = %d", alloc.LeaseEpoch)
	}
}
