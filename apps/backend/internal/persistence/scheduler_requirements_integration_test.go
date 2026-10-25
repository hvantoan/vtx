//go:build integration

package persistence_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/gitops"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/persistence/db"
	"github.com/vtx/platform/internal/requirement"
	"github.com/vtx/platform/internal/scheduler"
)

// Each test uses a private schema, so ledger assertions cannot depend on rows
// left by unrelated integration suites. Back up before changing the schema.
func schedulerDB(t *testing.T) (*pgxpool.Pool, *db.Queries) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	backup := exec.Command("pg_dump", "--dbname", dsn, "--file", filepath.Join(t.TempDir(), "before.sql"))
	if err := backup.Run(); err != nil {
		t.Fatalf("backup before schema change: %v", err)
	}
	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal("connect admin failed")
	}
	schema := fmt.Sprintf("vtx_s56_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer admin.Close(context.Background())
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated schema: %v", err)
		}
	})
	migrate := exec.Command("goose", "-dir", "../../db/migrations", "postgres", dsn, "up")
	migrate.Env = append(os.Environ(), "PGOPTIONS=-c search_path="+schema)
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("isolated migrations: %v (output bytes=%d)", err, len(out))
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse DB configuration failed")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal("connect isolated schema failed")
	}
	t.Cleanup(pool.Close)
	return pool, db.New(pool)
}
func seedRun(t *testing.T, q *db.Queries, n int) db.TaskRun {
	t.Helper()
	ctx := context.Background()
	key := fmt.Sprintf("VTX-%d", n)
	task, err := q.UpsertRegistryTask(ctx, db.UpsertRegistryTaskParams{JiraSiteID: "site", JiraIssueID: fmt.Sprint(n), JiraIssueKey: key, ProjectKey: "VTX"})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := q.InsertExecutionIntent(ctx, db.InsertExecutionIntentParams{Source: "jira_webhook", IssueKey: key, TargetStatusID: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	row, err := q.InsertTaskRun(ctx, db.InsertTaskRunParams{TaskID: task.ID, IntentID: intent.IntentID, CorrelationID: pgtype.Text{String: "correlation", Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertTaskRunEvent(ctx, db.InsertTaskRunEventParams{RunID: row.RunID, CurrentState: "queued", Action: "created", Actor: "registry", ActorType: "system"}); err != nil {
		t.Fatal(err)
	}
	return row
}
func TestSCH02TwoReplicasRespectAdmissionCap(t *testing.T) {
	pool, q := schedulerDB(t)
	ctx := context.Background()
	for n := 1; n <= 6; n++ {
		seedRun(t, q, n)
	}
	services := []*scheduler.Service{scheduler.New(persistence.NewPgSchedulerStore(pool, q), scheduler.Config{Owner: "one"}, nil, nil, nil), scheduler.New(persistence.NewPgSchedulerStore(pool, q), scheduler.Config{Owner: "two"}, nil, nil, nil)}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, svc := range services {
		wg.Add(1)
		go func(s *scheduler.Service) { defer wg.Done(); <-start; errs <- s.Tick(ctx) }(svc)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	live, err := q.CountLiveAdmissionSlots(ctx)
	if err != nil || live != 3 {
		t.Fatalf("live=%d err=%v", live, err)
	}
	queued, err := q.ListQueuedRunsForAdmission(ctx, 10)
	if err != nil || len(queued) != 3 {
		t.Fatalf("queued=%d err=%v", len(queued), err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE event_type='task_run.admitted.v1'").Scan(&count); err != nil || count != 3 {
		t.Fatalf("events=%d err=%v", count, err)
	}
	var max int
	if err := pool.QueryRow(ctx, "SELECT max((payload->>'live_slots')::int) FROM outbox_events WHERE event_type='task_run.admitted.v1'").Scan(&max); err != nil || max != 3 {
		t.Fatalf("event live_slots=%d err=%v", max, err)
	}
}
func TestSCH04GrantRenewAndExpiredTokenFenced(t *testing.T) {
	pool, q := schedulerDB(t)
	ctx := context.Background()
	r := seedRun(t, q, 1)
	owner := pgtype.Text{String: "worker", Valid: true}
	first, err := q.AcquireRunLease(ctx, db.AcquireRunLeaseParams{RunID: r.RunID, LeaseOwner: owner, LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.RenewRunLease(ctx, db.RenewRunLeaseParams{RunID: r.RunID, LeaseOwner: owner, LeaseEpoch: first.LeaseEpoch, LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(2 * time.Minute), Valid: true}}); err != nil {
		t.Fatal(err)
	}
	second, err := q.AcquireRunLease(ctx, db.AcquireRunLeaseParams{RunID: r.RunID, LeaseOwner: owner, LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, epoch := range []int64{first.LeaseEpoch, second.LeaseEpoch} {
		if _, err := q.RenewRunLease(ctx, db.RenewRunLeaseParams{RunID: r.RunID, LeaseOwner: owner, LeaseEpoch: epoch, LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("renew epoch=%d: %v", epoch, err)
		}
		registry := application.NewRegistryService(persistence.NewPgRegistryStore(pool, q), "registry", nil, nil)
		if _, err := registry.TransitionRun(ctx, application.TransitionCommand{RunID: r.RunID.String(), Action: domain.ActionBegin, Actor: domain.ActorScheduler, ExpectedLeaseEpoch: epoch}); !errors.Is(err, domain.ErrStaleLease) {
			t.Fatalf("transition epoch=%d: %v", epoch, err)
		}
	}
}
func TestSCH05ExpiredLeaseBlocksAndHoldsSlot(t *testing.T) {
	pool, q := schedulerDB(t)
	ctx := context.Background()
	r := seedRun(t, q, 1)
	if _, err := q.AcquireRunLease(ctx, db.AcquireRunLeaseParams{RunID: r.RunID, LeaseOwner: pgtype.Text{String: "lost", Valid: true}, LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.New(persistence.NewPgSchedulerStore(pool, q), scheduler.Config{}, nil, nil, nil).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := q.GetTaskRun(ctx, r.RunID)
	if err != nil || run.State != "blocked" || run.ErrorCode.String != "lease_lost" || run.LeaseOwner.Valid {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	live, err := q.CountLiveAdmissionSlots(ctx)
	if err != nil || live != 1 {
		t.Fatalf("live=%d err=%v", live, err)
	}
}
func cancelRun(t *testing.T, pool *pgxpool.Pool, q *db.Queries, id pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	r, err := q.GetTaskRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.NewRegistryService(persistence.NewPgRegistryStore(pool, q), "registry", nil, nil).TransitionRun(ctx, application.TransitionCommand{RunID: id.String(), ExpectedVersion: r.Version, ExpectedLeaseEpoch: r.LeaseEpoch, Action: domain.ActionCancel, Actor: domain.ActorOperator}); err != nil {
		t.Fatal(err)
	}
}
func TestSCH06DeferredPromotionFIFO(t *testing.T) {
	pool, q := schedulerDB(t)
	ctx := context.Background()
	r := seedRun(t, q, 1)
	ids := []pgtype.UUID{}
	for i := 1; i <= 2; i++ {
		intent, err := q.InsertExecutionIntent(ctx, db.InsertExecutionIntentParams{Source: "jira_webhook", IssueKey: "VTX-1", TargetStatusID: "ready"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, intent.IntentID)
		inbox, err := q.InsertJiraWebhookInbox(ctx, db.InsertJiraWebhookInboxParams{JiraSiteID: "site", IssueID: "1", IssueKey: "VTX-1", ChangelogID: fmt.Sprint(i), TargetStatusID: "ready", Outcome: "accepted", PayloadHash: fmt.Sprintf("%064d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.UpdateJiraWebhookInboxOutcome(ctx, db.UpdateJiraWebhookInboxOutcomeParams{EventID: inbox.EventID, Outcome: "accepted", IntentID: intent.IntentID}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.InsertDeferredIntent(ctx, db.InsertDeferredIntentParams{IntentID: intent.IntentID, TaskID: r.TaskID, Disposition: "pending_deferred"}); err != nil {
			t.Fatal(err)
		}
	}
	svc := scheduler.New(persistence.NewPgSchedulerStore(pool, q), scheduler.Config{}, nil, nil, nil)
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	for _, want := range ids {
		cancelRun(t, pool, q, r.RunID)
		if err := svc.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		var id pgtype.UUID
		if err := pool.QueryRow(ctx, "SELECT run_id FROM task_runs WHERE task_id=$1 AND state='preparing'", r.TaskID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		next, err := q.GetTaskRun(ctx, id)
		if err != nil || next.IntentID != want {
			t.Fatalf("FIFO intent=%s want=%s err=%v", next.IntentID.String(), want.String(), err)
		}
		r = next
	}
}
func TestSCH07AdmissionRollbackHasNoPartialLease(t *testing.T) {
	pool, q := schedulerDB(t)
	r := seedRun(t, q, 1)
	ctx := context.Background()
	injected := errors.New("simulated crash before commit")
	err := persistence.NewPgSchedulerStore(pool, q).Transaction(ctx, func(tx scheduler.Tx) error {
		if _, err := tx.Queued(ctx, 1); err != nil {
			return err
		}
		epoch, err := tx.Grant(ctx, r.RunID.String(), "worker", time.Now().Add(time.Minute))
		if err != nil {
			return err
		}
		if _, err := tx.Transition(ctx, application.TransitionCommand{RunID: r.RunID.String(), Action: domain.ActionBegin, Actor: domain.ActorScheduler, ExpectedLeaseEpoch: epoch}); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	run, err := q.GetTaskRun(ctx, r.RunID)
	if err != nil || run.State != "queued" || run.LeaseOwner.Valid || run.LeaseEpoch != 0 {
		t.Fatalf("partial admission %+v %v", run, err)
	}
}
func TestAllocateTwoQueuedRunsWithOneFreeSlot(t *testing.T) {
	pool, q := schedulerDB(t)
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		r := seedRun(t, q, i)
		if _, err := q.AcquireRunLease(ctx, db.AcquireRunLeaseParams{RunID: r.RunID, LeaseOwner: pgtype.Text{String: "existing", Valid: true}, LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
	a, b := seedRun(t, q, 3), seedRun(t, q, 4)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, r := range []db.TaskRun{a, b} {
		go func(r db.TaskRun) {
			<-start
			_, err := persistence.NewPgWorktreeStore(pool, q).AllocateWorktree(ctx, application.AllocateCommand{RunID: r.RunID.String(), Owner: "worker", RepoURL: "https://example.test/repo", LeaseExpiresAt: time.Now().Add(time.Minute)}, func(application.AllocateInput) (string, string, string, error) {
				return "/fixture/" + r.RunID.String(), "branch-" + r.RunID.String(), "base-sha", nil
			})
			errs <- err
		}(r)
	}
	close(start)
	success, full := 0, 0
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			success++
		} else if errors.Is(err, application.ErrAdmissionCap) {
			full++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || full != 1 {
		t.Fatalf("success=%d admission_full=%d", success, full)
	}
}

type fixtureIssue struct{}

func (fixtureIssue) FetchIssue(context.Context, string) (jira.FetchedIssue, error) {
	return jira.FetchedIssue{ID: "1", Key: "VTX-1", ProjectKey: "VTX", IssueContent: jira.IssueContent{Summary: "real scope", AcceptanceCriteria: "- preserve existing tests"}}, nil
}
func TestREQ04AndREQ05RealBaselineBoundToBaseSHA(t *testing.T) {
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			pool, q := schedulerDB(t)
			ctx := context.Background()
			r := seedRun(t, q, 1)
			if err := scheduler.New(persistence.NewPgSchedulerStore(pool, q), scheduler.Config{}, nil, nil, nil).Tick(ctx); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			path := filepath.Join(root, r.RunID.String())
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "Makefile"), []byte(fmt.Sprintf("test:\n\t@exit %d\n", exit)), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "Makefile"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "fixture"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = path
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git fixture: %v %s", err, out)
				}
			}
			sha, err := (gitops.Git{}).HeadSHA(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := q.InsertWorktree(ctx, db.InsertWorktreeParams{RunID: r.RunID, TaskID: r.TaskID, Path: path, Branch: "vtx/run-" + r.RunID.String(), RepoUrl: "https://example.test/repo", BaseSha: pgtype.Text{String: sha, Valid: true}}); err != nil {
				t.Fatal(err)
			}
			store := persistence.NewPgRequirementStore(pool, q)
			svc := requirement.New(store, fixtureIssue{}, requirement.ProcessRunner{Layout: gitops.Layout{WorktreeRoot: root}}, requirement.Config{}, nil)
			if _, err := svc.Extract(ctx, r.RunID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.RecordBaseline(ctx, r.RunID.String()); err != nil {
				t.Fatal(err)
			}
			dispatch, err := svc.Dispatch(ctx, r.RunID.String())
			if err != nil {
				t.Fatal(err)
			}
			want := "passed"
			if exit != 0 {
				want = "failed"
			}
			if dispatch.BaseSHA != sha || dispatch.Baseline.Status != want {
				t.Fatalf("dispatch=%+v", dispatch)
			}
			ready, err := q.HasPreparationEvidence(ctx, r.RunID)
			if err != nil || !ready {
				t.Fatalf("readiness=%v %v", ready, err)
			}
			registry := application.NewRegistryService(persistence.NewPgRegistryStore(pool, q), "registry", nil, nil)
			run, err := q.GetTaskRun(ctx, r.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := registry.TransitionRun(ctx, application.TransitionCommand{RunID: r.RunID.String(), ExpectedVersion: run.Version, ExpectedLeaseEpoch: run.LeaseEpoch, Action: domain.ActionAdvance, Actor: domain.ActorOrchestrator}); err != nil {
				t.Fatal(err)
			}
			view, err := persistence.NewPgRegistryStore(pool, q).GetRun(ctx, r.RunID.String())
			if err != nil || len(view.Requirement) == 0 || len(view.Baseline) == 0 {
				t.Fatalf("registry attachment: %v", err)
			}
			var packet map[string]json.RawMessage
			if err := json.Unmarshal(view.Requirement, &packet); err != nil {
				t.Fatal(err)
			}
			if len(packet["acceptance_criteria"]) == 0 {
				t.Fatal("registry scope missing")
			}
		})
	}
}
