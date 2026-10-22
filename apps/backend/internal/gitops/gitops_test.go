package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalPath_TraversalRejected(t *testing.T) {
	root := t.TempDir()
	if _, err := CanonicalPath(root, filepath.Join(root, "..", "escape")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

func TestCanonicalPath_AbsoluteOutsideRejected(t *testing.T) {
	root := t.TempDir()
	if _, err := CanonicalPath(root, "/tmp/elsewhere"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

func TestCanonicalPath_SymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := CanonicalPath(root, filepath.Join(link, "wt")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

func TestCanonicalPath_LeafSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "run-1")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := CanonicalPath(root, link); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
}

func TestCanonicalPath_MissingRootAllowed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wt") // never created
	got, err := CanonicalPath(root, filepath.Join(root, "run-1"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if filepath.Dir(got) != root {
		t.Fatalf("got %q", got)
	}
}

func TestCanonicalPath_AllowsChild(t *testing.T) {
	root := t.TempDir()
	got, err := CanonicalPath(root, filepath.Join(root, "child"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if filepath.Dir(got) != root {
		t.Fatalf("got %q", got)
	}
}

func TestRepoCacheKey_DeterministicAndDistinct(t *testing.T) {
	a := repoCacheKey("git@github.com:org/a.git")
	b := repoCacheKey("git@github.com:org/b.git")
	if a == b {
		t.Fatal("distinct urls share a cache key")
	}
	if a != repoCacheKey("git@github.com:org/a.git") {
		t.Fatal("cache key is not deterministic")
	}
	for _, r := range a {
		if r == '/' || r == ':' {
			t.Fatalf("unsafe char %q in key %q", r, a)
		}
	}
}

// Real-git lifecycle: clone a local source repo, add a worktree, dirty-check,
// remove. Uses the git binary — skipped when git is unavailable.
func TestGitLifecycle_RealRepo(t *testing.T) {
	if _, err := execLookPath(); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	src := t.TempDir()
	g := Git{}
	must := func(dir string, args ...string) {
		if _, err := g.run(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	must(src, "init", "-b", "main")
	must(src, "config", "user.email", "t@t")
	must(src, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(src, "add", ".")
	must(src, "commit", "-m", "init")

	layout := Layout{
		WorktreeRoot:   filepath.Join(t.TempDir(), "wt"),
		QuarantineRoot: filepath.Join(t.TempDir(), "q"),
		RepoCacheRoot:  filepath.Join(t.TempDir(), "cache"),
	}
	if err := os.MkdirAll(layout.WorktreeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cache, err := g.EnsureRepoCache(ctx, layout, src)
	if err != nil {
		t.Fatalf("ensure cache: %v", err)
	}
	wtPath := layout.WorktreePath("run-1")
	if err := g.AddWorktree(ctx, cache, wtPath, "vtx/T-1/run-1", "HEAD"); err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	sha, err := g.HeadSHA(ctx, wtPath)
	if err != nil || len(sha) != 40 {
		t.Fatalf("head sha = %q err %v", sha, err)
	}

	// Clean → not dirty → removable.
	dirty, err := g.IsDirty(ctx, wtPath)
	if err != nil || dirty {
		t.Fatalf("clean worktree dirty=%v err=%v", dirty, err)
	}
	if err := g.RemoveWorktree(ctx, cache, wtPath); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// Recreate, make it dirty (untracked file), expect quarantine not delete.
	if err := g.AddWorktree(ctx, cache, wtPath, "vtx/T-1/run-1", "HEAD"); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "junk.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err = g.IsDirty(ctx, wtPath)
	if err != nil || !dirty {
		t.Fatalf("untracked file not dirty: %v %v", dirty, err)
	}
	dest, err := g.Quarantine(ctx, layout, "run-1", wtPath, "test")
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "junk.txt")); err != nil {
		t.Fatalf("quarantined file missing: %v", err)
	}
	// The repo's worktree metadata must not reference the old path: repair
	// re-points it at the quarantine dest, prune drops it entirely.
	out, err := g.run(ctx, cache, "worktree", "list")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if strings.Contains(out, wtPath) {
		t.Fatalf("stale worktree entry for %s in:\n%s", wtPath, out)
	}
}

func execLookPath() (string, error) {
	return exec.LookPath("git")
}
