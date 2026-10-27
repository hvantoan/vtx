package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/execcmd"
	"github.com/vtx/platform/internal/gitops"
)

type verificationFixture struct {
	run        RunSnapshot
	path       string
	rows       []VerificationRun
	green      int
	superseded bool
	repinned   bool
	attempts   []int32
}

func (f *verificationFixture) VerificationContext(context.Context, string) (RunSnapshot, string, error) {
	return f.run, f.path, nil
}
func (f *verificationFixture) NextVerificationAttempt(context.Context, string) (int32, error) {
	return 1, nil
}
func (f *verificationFixture) BeginSuite(_ context.Context, _ VerifyCommand, r VerificationRun) (VerificationRun, bool, error) {
	f.attempts = append(f.attempts, r.Attempt)
	return r, true, nil
}
func (f *verificationFixture) FinishSuite(_ context.Context, _ VerifyCommand, r VerificationRun, green bool) error {
	f.rows = append(f.rows, r)
	if green {
		f.green++
	}
	return nil
}
func (f *verificationFixture) MarkSuperseded(context.Context, string, string) error {
	f.superseded = true
	return nil
}

func (f *verificationFixture) RepinCommit(_ context.Context, _ VerifyCommand, sha string) error {
	f.repinned = true
	f.run.CommitSHA = &sha
	return nil
}

type verificationHead struct {
	head     string
	ancestor bool
	dirty    bool
}

func (h verificationHead) HeadSHA(context.Context, string) (string, error) { return h.head, nil }

func (h verificationHead) IsAncestor(context.Context, string, string, string) (bool, error) {
	return h.ancestor, nil
}

func (h verificationHead) IsDirty(context.Context, string) (bool, error) { return h.dirty, nil }

type verificationRunner struct {
	calls  []execcmd.CmdSpec
	fail   string
	status string
}

func (r *verificationRunner) Run(_ context.Context, s execcmd.CmdSpec) (execcmd.Result, error) {
	r.calls = append(r.calls, s)
	if s.Name == r.fail {
		switch r.status {
		case "error":
			return execcmd.Result{}, errors.New("spawn")
		case "timeout":
			return execcmd.Result{TimedOut: true}, nil
		default:
			return execcmd.Result{ExitCode: 1}, nil
		}
	}
	return execcmd.Result{}, nil
}
func TestVerificationServiceContract(t *testing.T) {
	for _, tc := range []struct {
		name, fail, status string
		green, calls       int
	}{{"passed", "", "passed", 1, 5}, {"failed", "make-test", "failed", 0, 4}, {"timeout", "make-vet", "timeout", 0, 2}, {"spawn", "make-vet", "error", 0, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "run")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			sha, owner := "sha", "owner"
			expiry := time.Now().Add(time.Hour)
			f := &verificationFixture{path: path, run: RunSnapshot{RunID: "run", State: domain.StateVerifying, CommitSHA: &sha, LeaseOwner: &owner, LeaseEpoch: 1, LeaseExpiresAt: &expiry}}
			runner := &verificationRunner{fail: tc.fail, status: tc.status}
			s := VerificationService{Store: f, Runner: runner, Git: verificationHead{head: sha}, Layout: gitops.Layout{WorktreeRoot: root}}
			out, err := s.VerifyRun(context.Background(), VerifyCommand{RunID: "run", LeaseOwner: owner, LeaseEpoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			if out.Verdict != tc.status || f.green != tc.green || len(f.rows) != 5 || len(runner.calls) != tc.calls {
				t.Fatalf("out=%+v rows=%d green=%d calls=%d", out, len(f.rows), f.green, len(runner.calls))
			}
			for _, call := range runner.calls {
				if call.Dir != path || call.Argv[2] != path {
					t.Fatalf("not executing in run worktree: %+v", call)
				}
			}
			for _, row := range f.rows {
				if row.SHA != sha {
					t.Fatal("unbound evidence")
				}
			}
		})
	}
}
func TestVerificationServiceRejectsDriftAndUnsafePath(t *testing.T) {
	root := t.TempDir()
	owner, sha := "owner", "recorded"
	expiry := time.Now().Add(time.Hour)
	f := &verificationFixture{path: root, run: RunSnapshot{State: domain.StateVerifying, CommitSHA: &sha, LeaseOwner: &owner, LeaseEpoch: 1, LeaseExpiresAt: &expiry}}
	// CanonicalPath rejects the root itself and paths outside it.
	f.path = filepath.Join(root, "run")
	if err := os.Mkdir(f.path, 0700); err != nil {
		t.Fatal(err)
	}
	runner := &verificationRunner{}
	s := VerificationService{Store: f, Runner: runner, Git: verificationHead{head: "drift"}, Layout: gitops.Layout{WorktreeRoot: root}}
	out, err := s.VerifyRun(context.Background(), VerifyCommand{LeaseOwner: owner, LeaseEpoch: 1})
	if err != nil || out.Verdict != "stale_sha" || out.Repinned || !f.superseded || len(runner.calls) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	f.path = t.TempDir()
	if _, err := s.VerifyRun(context.Background(), VerifyCommand{LeaseOwner: owner, LeaseEpoch: 1}); err == nil {
		t.Fatal("unsafe cwd accepted")
	}
}

func TestVerificationServiceRepinsFastForwardHead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "run")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	pinned, head, owner := "pinned", "head", "owner"
	expiry := time.Now().Add(time.Hour)
	f := &verificationFixture{path: path, run: RunSnapshot{RunID: "run", State: domain.StateVerifying, CommitSHA: &pinned, LeaseOwner: &owner, LeaseEpoch: 1, LeaseExpiresAt: &expiry}}
	runner := &verificationRunner{}
	s := VerificationService{Store: f, Runner: runner, Git: verificationHead{head: head, ancestor: true}, Layout: gitops.Layout{WorktreeRoot: root}}
	out, err := s.VerifyRun(context.Background(), VerifyCommand{RunID: "run", LeaseOwner: owner, LeaseEpoch: 1, Attempt: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Repinned || out.Verdict != "passed" || out.SHA != head {
		t.Fatalf("out=%+v", out)
	}
	if !f.repinned || !f.superseded {
		t.Fatalf("repinned=%v superseded=%v", f.repinned, f.superseded)
	}
	if len(f.rows) != 5 {
		t.Fatalf("rows=%d", len(f.rows))
	}
	for _, row := range f.rows {
		if row.SHA != head {
			t.Fatalf("evidence bound to %s, want %s", row.SHA, head)
		}
		if row.Attempt != 1 {
			t.Fatalf("attempt=%d, want the next attempt after re-pin", row.Attempt)
		}
	}
}

func TestVerificationServiceRefusesDirtyWorktree(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "run")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	sha, owner := "recorded", "owner"
	expiry := time.Now().Add(time.Hour)
	f := &verificationFixture{path: path, run: RunSnapshot{RunID: "run", State: domain.StateVerifying, CommitSHA: &sha, LeaseOwner: &owner, LeaseEpoch: 1, LeaseExpiresAt: &expiry}}
	runner := &verificationRunner{}
	s := VerificationService{Store: f, Runner: runner, Git: verificationHead{head: sha, dirty: true}, Layout: gitops.Layout{WorktreeRoot: root}}
	out, err := s.VerifyRun(context.Background(), VerifyCommand{RunID: "run", LeaseOwner: owner, LeaseEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != "dirty_worktree" || out.AllPassed || len(runner.calls) != 0 || len(f.rows) != 0 {
		t.Fatalf("out=%+v calls=%d rows=%d", out, len(runner.calls), len(f.rows))
	}
}
