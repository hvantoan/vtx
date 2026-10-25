package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtx/platform/internal/domain"
)

type fakePRStore struct {
	run        PRRun
	verdict    GateVerdict
	verdictErr error
	saved      *SavePullRequest
	view       PullRequestView
	existing   *PullRequestView
}

func (f *fakePRStore) LoadPRRun(context.Context, string) (PRRun, error) { return f.run, nil }
func (f *fakePRStore) GetGateVerdict(context.Context, string, string) (GateVerdict, error) {
	return f.verdict, f.verdictErr
}
func (f *fakePRStore) GetPullRequestByRun(context.Context, string) (PullRequestView, error) {
	if f.existing != nil {
		return *f.existing, nil
	}
	return PullRequestView{}, ErrPRNotFound
}
func (f *fakePRStore) SaveOpenedPullRequest(_ context.Context, in SavePullRequest) (PullRequestView, error) {
	f.saved = &in
	return f.view, nil
}
func (f *fakePRStore) UpdatePullRequestSync(context.Context, string, string, string) (PullRequestView, error) {
	return PullRequestView{}, nil
}
func (f *fakePRStore) ListPullRequests(context.Context, *string, *time.Time, int32) ([]PullRequestView, error) {
	return nil, nil
}

type fakeGitHub struct {
	calls   int
	created *CreatePRInput
	headSHA string
}

func (f *fakeGitHub) GetRepo(context.Context, string, string) (RepoRef, error) {
	f.calls++
	return RepoRef{}, nil
}
func (f *fakeGitHub) GetRefSHA(_ context.Context, _, _, branch string) (string, error) {
	f.calls++
	if branch == "dev" {
		return "base", nil
	}
	return f.headSHA, nil
}
func (f *fakeGitHub) GetCommit(context.Context, string, string, string) (CommitInfo, error) {
	f.calls++
	return CommitInfo{}, nil
}
func (f *fakeGitHub) ListOpenPRsForHead(context.Context, string, string, string) ([]PRRef, error) {
	f.calls++
	return nil, nil
}
func (f *fakeGitHub) CreateDraftPR(_ context.Context, _, _ string, in CreatePRInput) (PRRef, error) {
	f.calls++
	f.created = &in
	return PRRef{ID: 1, Number: 7, URL: "https://github.com/acme/repo/pull/7", HeadSHA: f.headSHA, Draft: true, State: "open"}, nil
}
func (f *fakeGitHub) GetPR(context.Context, string, string, int) (PRRef, error) { return PRRef{}, nil }
func (f *fakeGitHub) GetCheckRollup(context.Context, string, string, string) (CheckRollup, error) {
	return CheckRollup{}, nil
}

type fakePRTransitions struct{ calls []TransitionCommand }

func (f *fakePRTransitions) TransitionRun(_ context.Context, cmd TransitionCommand) (RunSnapshot, error) {
	f.calls = append(f.calls, cmd)
	return RunSnapshot{RunID: cmd.RunID, Version: cmd.ExpectedVersion + 1}, nil
}

func testPRRun() PRRun {
	sha := "abc123"
	return PRRun{RunSnapshot: RunSnapshot{RunID: "00000000-0000-0000-0000-000000000001", TaskID: "00000000-0000-0000-0000-000000000002", IntentID: "00000000-0000-0000-0000-000000000003", State: domain.StateQualityGate, Version: 5, CommitSHA: &sha, CorrelationID: "corr"}, RepoURL: "https://github.com/acme/repo.git", Branch: "vtx/run", IssueKey: "VTX-12"}
}

func TestOpenDraftPRRefusesMissingOrFailingGateBeforeGitHub(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict GateVerdict
		err     error
	}{
		{"pending", GateVerdict{}, ErrGateEvidenceMissing},
		{"fail", GateVerdict{CommitSHA: "abc123", Decision: "fail"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakePRStore{run: testPRRun(), verdict: tc.verdict, verdictErr: tc.err}
			gh := &fakeGitHub{headSHA: "abc123"}
			transitions := &fakePRTransitions{}
			_, err := NewPRService(store, gh, transitions, "dev", nil).OpenDraftPR(context.Background(), OpenPRCommand{RunID: store.run.RunID, ExpectedVersion: 5})
			if !errors.Is(err, ErrGateEvidenceMissing) {
				t.Fatalf("error=%v, want gate precondition", err)
			}
			if gh.calls != 0 {
				t.Fatalf("GitHub called %d times before PASS", gh.calls)
			}
			if len(transitions.calls) != 0 {
				t.Fatalf("transition attempted before PASS")
			}
		})
	}
}

func TestOpenDraftPRBindsRequestToPassingRunAndSHA(t *testing.T) {
	run := testPRRun()
	store := &fakePRStore{run: run, verdict: GateVerdict{DecisionID: "gate-7", RunID: run.RunID, CommitSHA: "abc123", Decision: "pass"}, view: PullRequestView{RunID: run.RunID, Draft: true, Status: "open"}}
	gh := &fakeGitHub{headSHA: "abc123"}
	transitions := &fakePRTransitions{}
	got, err := NewPRService(store, gh, transitions, "dev", []string{"acme/repo"}).OpenDraftPR(context.Background(), OpenPRCommand{RunID: run.RunID, ExpectedVersion: 5, ExpectedLeaseEpoch: 9, Title: "[VTX-12] Draft PR", Body: "Quality gate passed."})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Draft || gh.created == nil {
		t.Fatalf("draft PR not created: %#v", got)
	}
	if gh.created.Base != "dev" || gh.created.Head != "vtx/run" || gh.created.Title != "[VTX-12] Draft PR" {
		t.Fatalf("request shape=%#v", gh.created)
	}
	for _, evidence := range []string{run.RunID, "abc123", "gate-7", "<!-- vtx-pipeline -->"} {
		if !strings.Contains(gh.created.Body, evidence) {
			t.Errorf("body missing %q: %s", evidence, gh.created.Body)
		}
	}
	if store.saved == nil || store.saved.HeadSHA != "abc123" || !store.saved.PR.Draft {
		t.Fatalf("saved=%#v", store.saved)
	}
	if len(transitions.calls) != 2 || transitions.calls[0].Action != domain.ActionAdvance || transitions.calls[1].Action != domain.ActionComplete {
		t.Fatalf("transitions=%#v", transitions.calls)
	}
}

func TestOpenDraftPRReplayWithPersistedPRCompletesRun(t *testing.T) {
	run := testPRRun()
	run.State = domain.StatePRDraft
	existing := PullRequestView{RunID: run.RunID, Draft: true, Status: "open"}
	store := &fakePRStore{run: run, verdict: GateVerdict{CommitSHA: "abc123", Decision: "pass"}, existing: &existing}
	gh := &fakeGitHub{headSHA: "abc123"}
	transitions := &fakePRTransitions{}
	got, err := NewPRService(store, gh, transitions, "dev", nil).OpenDraftPR(context.Background(), OpenPRCommand{RunID: run.RunID, ExpectedLeaseEpoch: 9})
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != run.RunID || len(transitions.calls) != 1 || transitions.calls[0].Action != domain.ActionComplete {
		t.Fatalf("got=%#v transitions=%#v", got, transitions.calls)
	}
	if gh.created != nil {
		t.Fatal("replay created a duplicate PR")
	}
}

func TestOpenDraftPRRefusesRemoteHeadDriftWithoutCreate(t *testing.T) {
	run := testPRRun()
	store := &fakePRStore{run: run, verdict: GateVerdict{CommitSHA: "abc123", Decision: "pass"}}
	gh := &fakeGitHub{headSHA: "new-sha"}
	_, err := NewPRService(store, gh, &fakePRTransitions{}, "dev", nil).OpenDraftPR(context.Background(), OpenPRCommand{RunID: run.RunID, ExpectedVersion: 5})
	if !errors.Is(err, ErrHeadSHAMismatch) {
		t.Fatalf("error=%v", err)
	}
	if gh.created != nil {
		t.Fatal("created PR for stale evidence")
	}
}
