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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/execcmd"
	"github.com/vtx/platform/internal/gitops"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/persistence/db"
	"github.com/vtx/platform/internal/qa"
	"github.com/vtx/platform/internal/retry"
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "TEST_DATABASE_URL unset: persistence integration tests skipped")
		os.Exit(0)
	}
	if out, err := exec.Command("goose", "-dir", "../../db/migrations", "postgres", dsn, "up").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "goose up: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type qualityFixture struct {
	t                                  *testing.T
	ctx                                context.Context
	pool                               *pgxpool.Pool
	q                                  *db.Queries
	run, task, intent, root, path, sha string
	registry                           *application.RegistryService
	store                              *persistence.PgRegistryStore
	guards                             persistence.EvidenceGuards
}

func gitCmd(t *testing.T, path string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = path
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}
func newQualityFixture(t *testing.T, state string) *qualityFixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &qualityFixture{t: t, ctx: ctx, pool: pool, q: db.New(pool), root: t.TempDir()}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q")
	gitCmd(t, repo, "config", "user.email", "test@vtx.local")
	gitCmd(t, repo, "config", "user.name", "VTX Test")
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte(".PHONY: test-boundaries vet typecheck test build\ntest-boundaries vet typecheck test build:\n\t@printf 'verified\\n'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "add", "Makefile")
	gitCmd(t, repo, "commit", "-qm", "fixture")
	f.sql(`INSERT INTO registry_tasks(jira_site_id,jira_issue_id,jira_issue_key,project_key) VALUES ('quality-test',replace(gen_random_uuid()::text,'-',''),'VTX-800','VTX') RETURNING id`, &f.task)
	f.sql(`INSERT INTO execution_intents(source,issue_key,target_status_id) VALUES ('test','VTX-800','1') RETURNING intent_id`, &f.intent)
	f.sql(`INSERT INTO task_runs(task_id,intent_id,state,lease_owner,lease_epoch,lease_expires_at,correlation_id) VALUES ($1,$2,$3,'worker',1,CURRENT_TIMESTAMP+INTERVAL '1 hour','quality-test') RETURNING run_id`, &f.run, f.task, f.intent, state)
	f.path = filepath.Join(f.root, f.run)
	gitCmd(t, repo, "worktree", "add", "-q", "-b", "run", f.path)
	sha, err := (gitops.Runner{}).HeadSHA(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.sha = sha
	f.exec(`UPDATE task_runs SET commit_sha=$2 WHERE run_id=$1`, f.run, sha)
	f.exec(`INSERT INTO worktrees(run_id,task_id,status,path,branch,repo_url) VALUES ($1,$2,'allocated',$3,'run','https://github.com/vtx/test')`, f.run, f.task, f.path)
	f.exec(`INSERT INTO task_run_events(run_id,previous_state,current_state,action,actor,actor_type,version,metadata) VALUES ($1,'',$2,'created','orchestrator','system',0,'{"coding_session_id":"coding"}')`, f.run, state)
	f.store = persistence.NewPgRegistryStore(pool, f.q)
	f.guards = persistence.EvidenceGuards{Head: gitops.Runner{}, WorktreeRoot: f.root}
	if err := f.store.WithTransitionGuards(f.guards.GuardMap()); err != nil {
		t.Fatal(err)
	}
	f.registry = application.NewRegistryService(f.store, "test", nil, nil).WithRetryPlanner(retry.DefaultPolicy())
	return f
}
func (f *qualityFixture) sql(query string, target any, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, query, args...).Scan(target); err != nil {
		f.t.Fatal(err)
	}
}
func (f *qualityFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *qualityFixture) count(table string) int {
	f.t.Helper()
	var n int
	f.sql("SELECT count(*) FROM "+table+" WHERE run_id=$1", &n, f.run)
	return n
}
func (f *qualityFixture) advance(version int64) (application.RunSnapshot, error) {
	return f.registry.TransitionRun(f.ctx, application.TransitionCommand{RunID: f.run, ExpectedVersion: version, ExpectedLeaseEpoch: 1, Action: domain.ActionAdvance, Actor: domain.ActorOrchestrator, ActorType: domain.ActorTypeSystem})
}
func (f *qualityFixture) verify() {
	f.t.Helper()
	s := application.VerificationService{Store: persistence.NewPgVerificationStore(f.pool, f.q), Runner: execcmd.ProcessRunner{}, Git: gitops.Runner{}, Layout: gitops.Layout{WorktreeRoot: f.root}, Sink: persistence.FileLogSink{Root: f.t.TempDir()}}
	out, err := s.VerifyRun(f.ctx, application.VerifyCommand{RunID: f.run, LeaseOwner: "worker", LeaseEpoch: 1})
	if err != nil || !out.AllPassed || out.SHA != f.sha {
		f.t.Fatalf("verification: %+v %v", out, err)
	}
	if f.count("verification_runs") != 5 || f.count("evidence_artifacts") != 5 {
		f.t.Fatal("missing suite/evidence rows")
	}
}

type qaRunner struct{}

func (qaRunner) StartSession(context.Context, qa.AgentWorkSpec) (string, error) {
	return "qa-independent", nil
}
func (qaRunner) CancelSession(context.Context, string, string) error { return nil }
func (f *qualityFixture) qaReady() (*qa.Service, qa.SubmitCommand) {
	f.t.Helper()
	store := persistence.NewPgQAStore(f.pool, f.q)
	s := &qa.Service{Store: store, Runner: qaRunner{}, Git: gitops.Runner{}, Layout: gitops.Layout{WorktreeRoot: f.root}}
	session, err := s.LaunchSession(f.ctx, qa.LaunchCommand{RunID: f.run, ExpectedVersion: 1, LeaseOwner: "worker", LeaseEpoch: 1})
	if err != nil {
		f.t.Fatal(err)
	}
	ev, err := store.RecordEvidence(f.ctx, qa.EvidenceArtifact{RunID: f.run, Attempt: 0, CommitSHA: f.sha, Kind: domain.EvidenceQATestRun, Producer: domain.ProducerQAAgent, SessionID: session.ID, URI: ".vtx/evidence/qa-tests.log", SHA256: "digest"})
	if err != nil {
		f.t.Fatal(err)
	}
	return s, qa.SubmitCommand{RunID: f.run, CommitSHA: f.sha, Verdict: domain.QAVerdictPass, QASessionID: session.ID, QAIdentity: "qa-agent", LeaseOwner: "worker", LeaseEpoch: 1, EvidenceIDs: []string{ev.ID}}
}
func TestQualityPipelineRealWorktreeAndConcurrentGuards(t *testing.T) {
	f := newQualityFixture(t, "verifying")
	if _, err := f.advance(0); !errors.Is(err, domain.ErrEvidenceInvalid) {
		t.Fatalf("advance without verification: %v", err)
	}
	f.verify()
	if _, err := f.advance(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.advance(1); !errors.Is(err, domain.ErrEvidenceInvalid) {
		t.Fatalf("advance without QA: %v", err)
	}
	s, c := f.qaReady()
	stale := c
	stale.CommitSHA = "stale-sha"
	if _, err := s.SubmitVerdict(f.ctx, stale); !errors.Is(err, domain.ErrQASHAMismatch) {
		t.Fatal(err)
	}
	if f.count("qa_verdicts") != 0 {
		t.Fatal("stale verdict persisted")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.SubmitVerdict(f.ctx, c); errs <- err }()
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, domain.ErrQAVerdictConflict):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 || f.count("qa_verdicts") != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	if _, err := f.advance(1); err != nil {
		t.Fatal(err)
	}
	gateService := persistence.NewPgGateService(f.pool, f.q, f.guards)
	d, err := gateService.Evaluate(f.ctx, f.run, 1)
	if err != nil || d.Decision != "pass" {
		t.Fatalf("gate: %+v %v", d, err)
	}
	errs = make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := f.advance(2); errs <- err }()
	}
	wg.Wait()
	close(errs)
	success, conflict = 0, 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, domain.ErrStaleVersion):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 19 {
		t.Fatalf("success=%d stale=%d", success, conflict)
	}
	var decisionID string
	f.sql(`SELECT metadata->>'gate_decision_id' FROM task_run_events WHERE run_id=$1 AND version=3`, &decisionID, f.run)
	if decisionID == "" {
		t.Fatal("missing gate audit id")
	}
	var events int
	f.sql(`SELECT count(*) FROM outbox_events WHERE intent_id=$1 AND event_type IN ('qa.verdict_recorded.v1','quality_gate.decision.v1')`, &events, f.intent)
	if events != 3 {
		t.Fatalf("evidence events=%d", events)
	}
}
func TestGateRejectsSHAMoveAfterRecordedPass(t *testing.T) {
	f := newQualityFixture(t, "verifying")
	f.verify()
	if _, err := f.advance(0); err != nil {
		t.Fatal(err)
	}
	s, c := f.qaReady()
	if _, err := s.SubmitVerdict(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.advance(1); err != nil {
		t.Fatal(err)
	}
	g := persistence.NewPgGateService(f.pool, f.q, f.guards)
	d, err := g.Evaluate(f.ctx, f.run, 1)
	if err != nil || d.Decision != "pass" {
		t.Fatalf("%+v %v", d, err)
	}
	gitCmd(t, f.path, "-c", "user.name=Test", "-c", "user.email=test@vtx.local", "commit", "--allow-empty", "-qm", "move")
	sha, err := (gitops.Runner{}).HeadSHA(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE task_runs SET commit_sha=$2 WHERE run_id=$1`, f.run, sha)
	if _, err := f.advance(2); !errors.Is(err, domain.ErrEvidenceInvalid) {
		t.Fatalf("stale PASS authorized PR: %v", err)
	}
	var version int
	f.sql(`SELECT version FROM task_runs WHERE run_id=$1`, &version, f.run)
	if version != 2 {
		t.Fatal("failed guard moved state")
	}
	d, err = g.Evaluate(f.ctx, f.run, 1)
	if err != nil || d.Decision != "fail" {
		t.Fatalf("%+v %v", d, err)
	}
}
func TestRetryRecoveryFencesAtomicallyAndRejectsStaleOrLiveLease(t *testing.T) {
	f := newQualityFixture(t, "coding")
	f.exec(`UPDATE task_runs SET lease_expires_at=CURRENT_TIMESTAMP-INTERVAL '1 minute' WHERE run_id=$1`, f.run)
	cmd := application.TransitionCommand{RunID: f.run, ExpectedVersion: 99, Action: domain.ActionScheduleRetry, Actor: domain.ActorPolicy, ActorType: domain.ActorTypeSystem, FailureClass: "lease_lost", RecoverExpiredLease: true}
	if _, err := f.registry.TransitionRun(f.ctx, cmd); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatal(err)
	}
	var epoch int
	f.sql(`SELECT lease_epoch FROM task_runs WHERE run_id=$1`, &epoch, f.run)
	if epoch != 1 {
		t.Fatal("stale CAS changed epoch")
	}
	cmd.ExpectedVersion = 0
	cmd.ActorType = domain.ActorTypeHuman
	if _, err := f.registry.TransitionRun(f.ctx, cmd); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	cmd.ActorType = domain.ActorTypeSystem
	run, err := f.registry.TransitionRun(f.ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateRetryWait || run.LeaseEpoch != 2 || run.LeaseOwner != nil || run.Attempt != 1 || run.ResumeState == nil || *run.ResumeState != domain.StateCoding || run.RetryDueAt == nil || !run.RetryDueAt.After(time.Now()) {
		t.Fatalf("%+v", run)
	}
	var metadata []byte
	f.sql(`SELECT metadata FROM task_run_events WHERE run_id=$1 AND version=1`, &metadata, f.run)
	var m map[string]any
	if err := json.Unmarshal(metadata, &m); err != nil {
		t.Fatal(err)
	}
	if m["failure_class"] != "lease_lost" || m["retry_policy_version"] != "v1" {
		t.Fatalf("%s", metadata)
	}
	live := newQualityFixture(t, "coding")
	cmd.RunID = live.run
	cmd.ExpectedVersion = 0
	if _, err := live.registry.TransitionRun(live.ctx, cmd); !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("recovered live lease: %v", err)
	}
	live.sql(`SELECT lease_epoch FROM task_runs WHERE run_id=$1`, &epoch, live.run)
	if epoch != 1 {
		t.Fatal("live epoch changed")
	}
}

type rejectAudit struct{}

func (rejectAudit) RecordDeliveryRedrive(context.Context, *db.Queries, string, string, string, string) error {
	return errors.New("audit unavailable")
}
func TestDLQRedriveAtomicAuditAndReplay(t *testing.T) {
	f := newQualityFixture(t, "coding")
	var eventID string
	f.sql(`INSERT INTO outbox_events(intent_id,event_type,payload,attempt,dead_lettered_at,dead_letter_reason) VALUES($1,'execution_intent.created.v1','{}',5,CURRENT_TIMESTAMP,'exhausted') RETURNING outbox_id`, &eventID, f.intent)
	s := persistence.NewPgRetryStore(f.pool, f.q)
	if _, err := s.RedriveDelivery(f.ctx, eventID, "registry", "policy", "retry", "corr"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	s.Audit = rejectAudit{}
	if _, err := s.RedriveDelivery(f.ctx, eventID, "registry", "operator", "retry", "corr"); err == nil {
		t.Fatal("audit failure ignored")
	}
	var dead bool
	f.sql(`SELECT dead_lettered_at IS NOT NULL FROM outbox_events WHERE outbox_id=$1`, &dead, eventID)
	if !dead {
		t.Fatal("failed audit cleared DLQ")
	}
	s.Audit = nil
	changed, err := s.RedriveDelivery(f.ctx, eventID, "registry", "operator", "retry", "corr")
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	changed, err = s.RedriveDelivery(f.ctx, eventID, "registry", "operator", "retry", "corr")
	if err != nil || changed {
		t.Fatalf("replay %v %v", changed, err)
	}
	var n int
	f.sql(`SELECT count(*) FROM delivery_redrives WHERE outbox_event_id=$1`, &n, eventID)
	if n != 1 {
		t.Fatal("missing/duplicate redrive audit")
	}
	f.sql(`SELECT count(*) FROM outbox_events WHERE intent_id=$1 AND event_type='outbox.delivery_redriven.v1'`, &n, f.intent)
	if n != 1 {
		t.Fatal("missing redrive outbox")
	}
}
