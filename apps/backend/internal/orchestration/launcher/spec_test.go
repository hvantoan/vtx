package launcher

import (
	"os"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/requirement"
)

const testRunID = "11111111-2222-4333-8444-555555555555"

func testLease(now time.Time) (*string, *time.Time) {
	owner := "worktree-service"
	expires := now.Add(2 * time.Minute)
	return &owner, &expires
}

func testInput(now time.Time, state domain.State) requirement.Input {
	owner, expires := testLease(now)
	base := "abc123"
	return requirement.Input{
		Run: application.RunSnapshot{
			RunID:          testRunID,
			TaskID:         "task-1",
			State:          state,
			Version:        4,
			Attempt:        1,
			LeaseOwner:     owner,
			LeaseEpoch:     3,
			LeaseExpiresAt: expires,
		},
		SiteID:     "site",
		IssueID:    "10014",
		IssueKey:   "VTX-5",
		ProjectKey: "VTX",
		Worktree: application.WorktreeView{
			RunID:   testRunID,
			Status:  application.WorktreeAllocated,
			Path:    "/tmp/worktrees/" + testRunID,
			Branch:  "vtx/run-" + testRunID,
			RepoURL: "git@example.test:vtx.git",
			BaseSHA: &base,
		},
	}
}

func testPacket() requirement.Packet {
	return requirement.Packet{
		IssueKey:           "VTX-5",
		Summary:            "Add spinner",
		AcceptanceCriteria: []string{"spinner spins"},
	}
}

func testBaseline() requirement.Baseline {
	return requirement.Baseline{Suite: "make test", Status: "passed"}
}

func TestBuildSpecCarriesFencedLeaseAndInvariants(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	spec, err := BuildSpec(testInput(now, domain.StateCoding), testPacket(), testBaseline(), Config{Owner: "worktree-service"})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if spec.RunID != testRunID || spec.Goal != "Add spinner" {
		t.Fatalf("identity/goal wrong: %+v", spec)
	}
	// The orchestrator must not guess versions: the launch-time version/state is
	// the CAS baseline for its first transition.
	if spec.RunVersion != 4 || spec.RunState != string(domain.StateCoding) {
		t.Fatalf("run version/state not carried: %d %q", spec.RunVersion, spec.RunState)
	}
	if spec.Lease.Owner != "worktree-service" || spec.Lease.Epoch != 3 {
		t.Fatalf("lease not fenced: %+v", spec.Lease)
	}
	if spec.Lease.ExpiresAt != now.Add(2*time.Minute).Format(time.RFC3339) {
		t.Fatalf("lease expiry wrong: %q", spec.Lease.ExpiresAt)
	}
	if !spec.Delegation.QAIndependent {
		t.Fatal("qa_independent must be true")
	}
	if spec.Delegation.MaxParallel != 3 {
		t.Fatalf("max_parallel must equal the admission cap: %d", spec.Delegation.MaxParallel)
	}
	if spec.Repo.WorktreePath != "/tmp/worktrees/"+testRunID || spec.Repo.BaseRef != "abc123" {
		t.Fatalf("repo binding wrong: %+v", spec.Repo)
	}
	if spec.TestBaseline != "make test (baseline: passed)" {
		t.Fatalf("test baseline wrong: %q", spec.TestBaseline)
	}
	for _, tool := range []string{"vtx_gate", "vtx_open_pr"} {
		if !contains(spec.AllowedTools, tool) {
			t.Fatalf("allowed tools missing %s: %v", tool, spec.AllowedTools)
		}
	}
	if contains(spec.Delegation.Agents, "vtx-qa") {
		t.Fatal("QA must not be a delegation target")
	}
}

func TestBuildSpecFailsWithoutLease(t *testing.T) {
	in := testInput(time.Now(), domain.StateCoding)
	in.Run.LeaseOwner = nil
	if _, err := BuildSpec(in, testPacket(), testBaseline(), Config{}); err == nil {
		t.Fatal("expected error for a run with no lease")
	}
}

func TestBuildSpecFallsBackToDescription(t *testing.T) {
	p := testPacket()
	p.Summary = ""
	p.Description = "described work"
	spec, err := BuildSpec(testInput(time.Now(), domain.StateCoding), p, testBaseline(), Config{})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if spec.Goal != "described work" {
		t.Fatalf("goal fallback wrong: %q", spec.Goal)
	}
}

func TestSessionIDIsFencedPerEpoch(t *testing.T) {
	spec, err := BuildSpec(testInput(time.Now(), domain.StateCoding), testPacket(), testBaseline(), Config{})
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if got, want := SessionID(spec), testRunID+"-e3"; got != want {
		t.Fatalf("SessionID = %q, want %q", got, want)
	}
	spec.Lease.Epoch++
	if got := SessionID(spec); got == testRunID+"-e3" {
		t.Fatal("a new lease epoch must start a fresh Pi session")
	}
}

func TestWriteSpecRejectsTraversal(t *testing.T) {
	if _, err := WriteSpec(t.TempDir(), Spec{RunID: "../../etc/passwd"}); err == nil {
		t.Fatal("expected invalid run id error")
	}
}

func TestWriteSpecIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteSpec(dir, Spec{RunID: testRunID})
	if err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spec mode = %o, want 600", info.Mode().Perm())
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
