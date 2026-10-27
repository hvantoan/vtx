package requirement

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/integrations/jira"
)

func TestREQ01CriteriaPrecedenceAndCanonicalHash(t *testing.T) {
	for _, tc := range []struct {
		name, custom, description string
		want                      []string
	}{
		{"custom", "- explicit", "## Acceptance Criteria\n- fallback", []string{"explicit"}},
		{"heading", "", "summary\n## Acceptance Criteria\n- first\n- second\n## Notes\nno", []string{"first", "second"}},
		{"checklist", "", "description\n- [ ] checked\n- ordinary", []string{"checked"}},
		{"none", "", "only a summary", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := jira.FetchedIssue{Key: "VTX-6", IssueContent: jira.IssueContent{Summary: "scope", Description: tc.description, AcceptanceCriteria: tc.custom}}
			p, err := Normalize(issue)
			if err != nil {
				t.Fatal(err)
			}
			q, err := Normalize(issue)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(p.AcceptanceCriteria)
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) || p.RequirementHash != q.RequirementHash || len(p.RequirementHash) != 64 {
				t.Fatalf("packet=%+v want=%s", p, want)
			}
			issue.Summary = "new scope"
			changed, err := Normalize(issue)
			if err != nil {
				t.Fatal(err)
			}
			if changed.RequirementHash == p.RequirementHash {
				t.Fatal("scope change did not alter hash")
			}
		})
	}
}
func TestREQ02NoAcceptanceCriteriaSynthesized(t *testing.T) {
	p, err := Normalize(jira.FetchedIssue{IssueContent: jira.IssueContent{Summary: "Fix bug"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.AcceptanceCriteria == nil || len(p.AcceptanceCriteria) != 0 {
		t.Fatalf("criteria=%v", p.AcceptanceCriteria)
	}
}

type memoryRequirements struct {
	input    Input
	packet   Packet
	exists   bool
	baseline Baseline
	blocked  string
	saved    int
}

func (m *memoryRequirements) Input(context.Context, string) (Input, error) { return m.input, nil }
func (m *memoryRequirements) Requirement(context.Context, string) (Packet, bool, error) {
	return m.packet, m.exists, nil
}
func (m *memoryRequirements) SaveRequirement(_ context.Context, _ Input, p Packet, _ time.Time) (Packet, error) {
	m.packet = p
	m.exists = true
	m.saved++
	return p, nil
}
func (m *memoryRequirements) Baseline(ctx context.Context, _ Input, command string, _ time.Time, run func(context.Context) (SuiteResult, error)) (Baseline, error) {
	if m.baseline.Suite != "" {
		return m.baseline, nil
	}
	result, err := run(ctx)
	status := "passed"
	if result.ExitCode != 0 {
		status = "failed"
	}
	if result.TimedOut || err != nil {
		status = "inconclusive"
	}
	m.baseline = Baseline{Suite: command, Status: status, ArtifactIDs: []string{}}
	return m.baseline, nil
}
func (m *memoryRequirements) ReadBaseline(context.Context, Input) (Baseline, error) {
	if m.baseline.Suite == "" {
		return Baseline{}, domain.ErrEvidenceInvalid
	}
	return m.baseline, nil
}
func (m *memoryRequirements) Block(_ context.Context, _ Input, code string) error {
	m.blocked = code
	return nil
}
func (m *memoryRequirements) Renew(context.Context, Input, time.Time) error { return nil }

type fakeJira struct {
	issue jira.FetchedIssue
	calls int
}

func (f *fakeJira) FetchIssue(context.Context, string) (jira.FetchedIssue, error) {
	f.calls++
	return f.issue, nil
}

type suiteRunner struct {
	result SuiteResult
	err    error
	calls  int
}

func (r *suiteRunner) RunBaseline(context.Context, application.WorktreeView, string) (SuiteResult, error) {
	r.calls++
	return r.result, r.err
}
func preparedStore() *memoryRequirements {
	return &memoryRequirements{input: Input{Run: application.RunSnapshot{RunID: "run", State: domain.StatePreparing, LeaseOwner: new("worker"), LeaseEpoch: 1}, IssueID: "1006", IssueKey: "VTX-6", ProjectKey: "VTX", Worktree: application.WorktreeView{Status: "allocated", Path: "/work/run", Branch: "vtx/run-run", BaseSHA: new("base-sha")}}}
}
func TestREQ03JiraErrorsBoundedAndClassified(t *testing.T) {
	for _, tc := range []struct {
		status, calls int
		code          string
	}{{401, 1, "jira_auth"}, {403, 1, "jira_auth"}, {404, 1, "jira_issue_gone"}, {500, 3, "jira_fetch_failed"}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(tc.status) }))
			defer upstream.Close()
			client, err := jira.NewClient(upstream.URL, "fixture", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			store := preparedStore()
			_, err = New(store, client, nil, Config{}, nil).Extract(context.Background(), "run")
			if err == nil || calls != tc.calls || store.blocked != tc.code || store.saved != 0 {
				t.Fatalf("err=%v calls=%d block=%s", err, calls, store.blocked)
			}
		})
	}
}
func TestREQ05FailedBaselineDispatchesFlaggedAndREQ08Replay(t *testing.T) {
	store := preparedStore()
	fetcher := &fakeJira{issue: jira.FetchedIssue{ID: "1006", Key: "VTX-6", ProjectKey: "VTX", IssueContent: jira.IssueContent{Summary: "scope", AcceptanceCriteria: "- check"}}}
	runner := &suiteRunner{result: SuiteResult{ExitCode: 1}}
	svc := New(store, fetcher, runner, Config{}, nil)
	if _, err := svc.Extract(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Extract(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordBaseline(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordBaseline(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	dispatch, err := svc.Dispatch(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Baseline.Status != "failed" || fetcher.calls != 1 || runner.calls != 1 || store.saved != 1 {
		t.Fatalf("dispatch=%+v fetch=%d execute=%d saves=%d", dispatch, fetcher.calls, runner.calls, store.saved)
	}
	raw, err := json.Marshal(dispatch)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"worktree_path", "branch", "base_sha", "requirement_version", "requirement_hash", "acceptance_criteria", "baseline", "artifact_ids"} {
		if !strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("missing field %s in %s", field, raw)
		}
	}
}
func TestBaselineTimeoutBlocksRatherThanForgingDispatch(t *testing.T) {
	store := preparedStore()
	store.exists = true
	runner := &suiteRunner{result: SuiteResult{TimedOut: true}}
	svc := New(store, nil, runner, Config{}, nil)
	if _, err := svc.RecordBaseline(context.Background(), "run"); err == nil || store.blocked != "baseline_infra" {
		t.Fatalf("err=%v blocked=%s", err, store.blocked)
	}
	if _, err := svc.Dispatch(context.Background(), "run"); !errors.Is(err, domain.ErrEvidenceInvalid) {
		t.Fatalf("dispatch err=%v", err)
	}
}
func TestSnapshotRedactsSecrets(t *testing.T) {
	packet, err := Normalize(jira.FetchedIssue{IssueContent: jira.IssueContent{Summary: "scope", Description: "password=fixture-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "fixture-secret") {
		t.Fatal("snapshot retained credentials")
	}
}
