//go:build integration

// VTX-4 migration compatibility: 00005 must apply cleanly over a POPULATED
// VTX-3 schema and be restart-safe (down-to then up loses nothing in VTX-3
// tables). Runs inside the shared db integration suite — TestMain already
// proves the full up path.
package db

import (
	"context"
	"testing"
)

func TestWorktreeMigration_PopulatedVTX3Schema(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Seed a populated VTX-3 chain: task → intent → run → event. These rows
	// must survive the worktree migration untouched.
	var taskID, intentID, runID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO registry_tasks (jira_site_id, jira_issue_id, jira_issue_key, project_key)
		 VALUES ('site', 'mig-1', 'VTX-900', 'VTX') RETURNING id`).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO execution_intents (source, issue_key, target_status_id)
		 VALUES ('test', 'VTX-900', 's') RETURNING intent_id`).Scan(&intentID); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO task_runs (task_id, intent_id, state, lease_owner, lease_epoch)
		 VALUES ($1, $2, 'preparing', 'w', 3) RETURNING run_id`, taskID, intentID).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Roll the worktree migration back and forward on live data.
	goose(t, "down-to", "4")
	goose(t, "up")

	// VTX-3 rows intact, lease fields preserved.
	var owner string
	var epoch int64
	if err := pool.QueryRow(ctx,
		`SELECT lease_owner, lease_epoch FROM task_runs WHERE run_id = $1`, runID,
	).Scan(&owner, &epoch); err != nil {
		t.Fatalf("run lost after migration cycle: %v", err)
	}
	if owner != "w" || epoch != 3 {
		t.Fatalf("lease fields changed: %s/%d", owner, epoch)
	}
	// New surfaces exist: repo_url column and the worktrees table.
	var repoURL *string
	if err := pool.QueryRow(ctx,
		`SELECT repo_url FROM registry_tasks WHERE id = $1`, taskID).Scan(&repoURL); err != nil {
		t.Fatalf("repo_url column missing: %v", err)
	}
	if repoURL != nil {
		t.Fatalf("repo_url should be NULL for a pre-VTX-4 task, got %q", *repoURL)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM worktrees`).Scan(&n); err != nil {
		t.Fatalf("worktrees table missing: %v", err)
	}
}
