package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Fake store: runs the effect like the real tx would, but keeps state in maps.
type fakeWorktreeStore struct {
	worktrees   map[string]WorktreeView // run_id → row
	allocated   []AllocatedWorktreeView
	allocateErr error
	leaseState  struct {
		owner *string
		epoch int64
	}
	effectRan int
}

func (f *fakeWorktreeStore) AllocateWorktree(ctx context.Context, cmd AllocateCommand, effect func(AllocateInput) (string, string, string, error)) (Allocation, error) {
	if f.allocateErr != nil {
		return Allocation{}, f.allocateErr
	}
	if wt, ok := f.worktrees[cmd.RunID]; ok {
		return Allocation{Worktree: wt, Existing: true}, nil
	}
	in := AllocateInput{RunID: cmd.RunID, TaskID: "task-1", State: "preparing", RepoURL: cmd.RepoURL, JiraIssueKey: "VTX-4"}
	path, branch, sha, err := effect(in)
	f.effectRan++
	if err != nil {
		return Allocation{}, err
	}
	wt := WorktreeView{
		WorktreeID: "wt-" + cmd.RunID, RunID: cmd.RunID, TaskID: in.TaskID,
		Status: WorktreeAllocated, Path: path, Branch: branch, RepoURL: in.RepoURL,
		BaseSHA: &sha, CreatedAt: time.Now(),
	}
	f.worktrees[cmd.RunID] = wt
	return Allocation{Worktree: wt, LeaseOwner: cmd.Owner, LeaseEpoch: 1}, nil
}

func (f *fakeWorktreeStore) GetWorktree(ctx context.Context, runID string) (WorktreeView, error) {
	wt, ok := f.worktrees[runID]
	if !ok {
		return WorktreeView{}, ErrWorktreeNotFound
	}
	return wt, nil
}

func (f *fakeWorktreeStore) CompleteCleanup(ctx context.Context, id, from, to, reason string) (WorktreeView, error) {
	for k, wt := range f.worktrees {
		if wt.WorktreeID == id && wt.Status == from {
			wt.Status = to
			wt.QuarantineReason = &reason
			f.worktrees[k] = wt
			return wt, nil
		}
	}
	return WorktreeView{}, ErrWorktreeNotFound
}

func (f *fakeWorktreeStore) ListAllocatedWorktrees(ctx context.Context) ([]AllocatedWorktreeView, error) {
	return f.allocated, nil
}

func (f *fakeWorktreeStore) RunLeaseState(ctx context.Context, runID string) (*string, int64, *time.Time, string, error) {
	return f.leaseState.owner, f.leaseState.epoch, nil, "preparing", nil
}

// Fake git.
type fakeGit struct {
	repoDir     string
	addErr      error
	ensureErr   error
	dirty       map[string]bool
	dirtyErr    map[string]error
	removed     []string
	quarantined []string
	orphans     []string
	paths       map[string]string
}

func (f *fakeGit) EnsureRepo(ctx context.Context, repoURL string) (string, error) {
	return f.repoDir, f.ensureErr
}
func (f *fakeGit) Add(ctx context.Context, repoDir, runID, branch, baseRef string) (string, error) {
	if f.addErr != nil {
		return "", f.addErr
	}
	p := "/root/" + runID
	f.paths[runID] = p
	return p, nil
}
func (f *fakeGit) HeadSHA(ctx context.Context, path string) (string, error) { return "deadbeef", nil }
func (f *fakeGit) IsDirty(ctx context.Context, path string) (bool, error) {
	if err, ok := f.dirtyErr[path]; ok {
		return false, err
	}
	return f.dirty[path], nil
}
func (f *fakeGit) Remove(ctx context.Context, repoDir, path string) error {
	f.removed = append(f.removed, path)
	return nil
}
func (f *fakeGit) Quarantine(ctx context.Context, runID, path, reason string) (string, error) {
	f.quarantined = append(f.quarantined, path)
	return "/q/" + runID, nil
}
func (f *fakeGit) OrphanDirs(ctx context.Context, known map[string]bool) ([]string, error) {
	return f.orphans, nil
}

func newFakeGit() *fakeGit {
	return &fakeGit{repoDir: "/cache/repo", dirty: map[string]bool{}, dirtyErr: map[string]error{}, paths: map[string]string{}}
}

func newTestService(store *fakeWorktreeStore, git *fakeGit) *WorktreeService {
	return NewWorktreeService(store, git, "test-owner", time.Minute, nil, nil)
}

func TestAllocateWorktree_Success(t *testing.T) {
	store := &fakeWorktreeStore{worktrees: map[string]WorktreeView{}}
	svc := newTestService(store, newFakeGit())
	alloc, err := svc.AllocateWorktree(context.Background(), AllocateCommand{RunID: "run-1", RepoURL: "git@x:y"})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if alloc.Existing {
		t.Fatal("fresh allocation reported as existing")
	}
	if alloc.Worktree.Branch != "vtx/VTX-4/run-1" {
		t.Fatalf("branch = %q", alloc.Worktree.Branch)
	}
	if alloc.LeaseEpoch != 1 || alloc.LeaseOwner != "test-owner" {
		t.Fatalf("lease = %s/%d", alloc.LeaseOwner, alloc.LeaseEpoch)
	}
}

func TestAllocateWorktree_IdempotentRetry(t *testing.T) {
	store := &fakeWorktreeStore{worktrees: map[string]WorktreeView{}}
	git := newFakeGit()
	svc := newTestService(store, git)
	first, err := svc.AllocateWorktree(context.Background(), AllocateCommand{RunID: "run-1", RepoURL: "r"})
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	second, err := svc.AllocateWorktree(context.Background(), AllocateCommand{RunID: "run-1", RepoURL: "r"})
	if err != nil {
		t.Fatalf("retry allocate: %v", err)
	}
	if !second.Existing || second.Worktree.WorktreeID != first.Worktree.WorktreeID {
		t.Fatal("retry did not return the existing row")
	}
	if store.effectRan != 1 {
		t.Fatalf("effect ran %d times on retry", store.effectRan)
	}
}

func TestAllocateWorktree_NoRepoURL(t *testing.T) {
	store := &fakeWorktreeStore{worktrees: map[string]WorktreeView{}}
	svc := newTestService(store, newFakeGit())
	_, err := svc.AllocateWorktree(context.Background(), AllocateCommand{RunID: "run-1"})
	if !errors.Is(err, ErrWorktreeNoRepo) {
		t.Fatalf("err = %v, want ErrWorktreeNoRepo", err)
	}
}

func TestAllocateWorktree_TerminalRun(t *testing.T) {
	store := &fakeWorktreeStore{worktrees: map[string]WorktreeView{}, allocateErr: ErrWorktreeRunTerminal}
	svc := newTestService(store, newFakeGit())
	_, err := svc.AllocateWorktree(context.Background(), AllocateCommand{RunID: "run-1", RepoURL: "r"})
	if !errors.Is(err, ErrWorktreeRunTerminal) {
		t.Fatalf("err = %v", err)
	}
}

func TestAllocateWorktree_GitFailureLeavesNoRow(t *testing.T) {
	store := &fakeWorktreeStore{worktrees: map[string]WorktreeView{}}
	git := newFakeGit()
	git.addErr = errors.New("disk full")
	svc := newTestService(store, git)
	_, err := svc.AllocateWorktree(context.Background(), AllocateCommand{RunID: "run-1", RepoURL: "r"})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(store.worktrees) != 0 {
		t.Fatal("worktree row left behind after git failure")
	}
}

func TestReleaseWorktree_DirtyQuarantines(t *testing.T) {
	store := &fakeWorktreeStore{worktrees: map[string]WorktreeView{
		"run-1": {WorktreeID: "wt-1", RunID: "run-1", Status: WorktreeAllocated, Path: "/root/run-1", RepoURL: "r"},
	}}
	git := newFakeGit()
	git.dirty["/root/run-1"] = true
	svc := newTestService(store, git)
	wt, err := svc.ReleaseWorktree(context.Background(), "run-1", "run finished")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if wt.Status != WorktreeQuarantined {
		t.Fatalf("status = %s", wt.Status)
	}
	if len(git.quarantined) != 1 || len(git.removed) != 0 {
		t.Fatalf("quarantine=%v removed=%v", git.quarantined, git.removed)
	}
}

func TestReleaseWorktree_CleanRemoves(t *testing.T) {
	store := &fakeWorktreeStore{worktrees: map[string]WorktreeView{
		"run-1": {WorktreeID: "wt-1", RunID: "run-1", Status: WorktreeAllocated, Path: "/root/run-1", RepoURL: "r"},
	}}
	git := newFakeGit()
	svc := newTestService(store, git)
	wt, err := svc.ReleaseWorktree(context.Background(), "run-1", "done")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if wt.Status != WorktreeReleased || len(git.removed) != 1 {
		t.Fatalf("status=%s removed=%v", wt.Status, git.removed)
	}
}

func TestReconcile_LiveLeaseUntouched(t *testing.T) {
	future := time.Now().Add(time.Hour)
	store := &fakeWorktreeStore{
		worktrees: map[string]WorktreeView{},
		allocated: []AllocatedWorktreeView{{
			WorktreeView:     WorktreeView{WorktreeID: "wt-1", RunID: "run-1", Status: "allocated", Path: "/root/run-1"},
			RunState:         "coding",
			RunLeaseExpireAt: &future,
		}},
	}
	git := newFakeGit()
	svc := newTestService(store, git)
	rep, err := svc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Released+rep.Quarantined != 0 || len(git.removed)+len(git.quarantined) != 0 {
		t.Fatalf("live lease was reclaimed: %+v", rep)
	}
}

func TestReconcile_ExpiredLeaseReclaimed(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	store := &fakeWorktreeStore{
		worktrees: map[string]WorktreeView{
			"run-1": {WorktreeID: "wt-1", RunID: "run-1", Status: WorktreeAllocated, Path: "/root/run-1", RepoURL: "r"},
		},
		allocated: []AllocatedWorktreeView{{
			WorktreeView:     WorktreeView{WorktreeID: "wt-1", RunID: "run-1", Status: "allocated", Path: "/root/run-1", RepoURL: "r"},
			RunState:         "coding",
			RunLeaseExpireAt: &past,
		}},
	}
	git := newFakeGit()
	svc := newTestService(store, git)
	rep, err := svc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Released != 1 {
		t.Fatalf("released = %d, errors=%v", rep.Released, rep.Errors)
	}
}

func TestReconcile_OrphanDirQuarantined(t *testing.T) {
	store := &fakeWorktreeStore{worktrees: map[string]WorktreeView{}}
	git := newFakeGit()
	git.orphans = []string{"/root/dead-run"}
	svc := newTestService(store, git)
	rep, err := svc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.OrphansQuarantined != 1 || len(git.quarantined) != 1 {
		t.Fatalf("orphans=%d quarantined=%v", rep.OrphansQuarantined, git.quarantined)
	}
}

func TestBranchFor(t *testing.T) {
	cases := []struct{ issue, run, want string }{
		{"VTX-4", "abcd1234-ffff", "vtx/VTX-4/abcd1234"},
		{"", "run", "vtx/task/run"},
		{"weird key!!", "r1", "vtx/weird-key/r1"},
	}
	for _, c := range cases {
		if got := BranchFor(c.issue, c.run); got != c.want {
			t.Errorf("BranchFor(%q,%q) = %q, want %q", c.issue, c.run, got, c.want)
		}
	}
}
