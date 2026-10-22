package gitops

import (
	"context"
	"fmt"
)

// Runner adapts Git+Layout to the application's WorktreeGit port: every path
// is canonicalized against the layout roots before it reaches the filesystem.
type Runner struct {
	Git    Git
	Layout Layout
}

var _ interface {
	EnsureRepo(ctx context.Context, repoURL string) (string, error)
} = Runner{}

func (r Runner) EnsureRepo(ctx context.Context, repoURL string) (string, error) {
	return r.Git.EnsureRepoCache(ctx, r.Layout, repoURL)
}

func (r Runner) Add(ctx context.Context, repoDir, runID, branch, baseRef string) (string, error) {
	path, err := CanonicalPath(r.Layout.WorktreeRoot, r.Layout.WorktreePath(runID))
	if err != nil {
		return "", err
	}
	if err := r.Git.AddWorktree(ctx, repoDir, path, branch, baseRef); err != nil {
		return "", err
	}
	return path, nil
}

func (r Runner) HeadSHA(ctx context.Context, path string) (string, error) {
	return r.Git.HeadSHA(ctx, path)
}

func (r Runner) IsDirty(ctx context.Context, path string) (bool, error) {
	return r.Git.IsDirty(ctx, path)
}

func (r Runner) Remove(ctx context.Context, repoDir, path string) error {
	if _, err := CanonicalPath(r.Layout.WorktreeRoot, path); err != nil {
		return err
	}
	return r.Git.RemoveWorktree(ctx, repoDir, path)
}

func (r Runner) Quarantine(ctx context.Context, runID, path, reason string) (string, error) {
	if _, err := CanonicalPath(r.Layout.WorktreeRoot, path); err != nil {
		return "", err
	}
	return r.Git.Quarantine(ctx, r.Layout, runID, path, reason)
}

// OrphanDirs returns worktree-root child dirs whose names are not in known.
// Callers pass the set of run ids that have DB rows; anything left over is a
// crash-window orphan and gets quarantined, never deleted.
func (r Runner) OrphanDirs(ctx context.Context, known map[string]bool) ([]string, error) {
	_ = ctx
	dirs, err := ListWorktreeDirs(r.Layout.WorktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("list worktree root: %w", err)
	}
	var out []string
	for _, name := range dirs {
		if !known[name] {
			out = append(out, r.Layout.WorktreePath(name))
		}
	}
	return out, nil
}
