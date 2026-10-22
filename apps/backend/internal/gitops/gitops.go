// Package gitops is VTX-4's disk boundary: every git invocation and filesystem
// mutation for worktree lifecycle goes through Git. The service decides WHEN
// to do things; this package decides that they are done safely.
package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Layout is the canonical on-disk contract. Every path the service produces
// must derive from these roots — never from caller-supplied absolute paths.
type Layout struct {
	// WorktreeRoot holds one child directory per run: <WorktreeRoot>/<run_id>.
	WorktreeRoot string
	// QuarantineRoot receives dirty worktrees moved aside instead of deleted.
	QuarantineRoot string
	// RepoCacheRoot holds one clone per repo_url: <RepoCacheRoot>/<url-key>.
	RepoCacheRoot string
}

// WorktreePath returns the deterministic allocation path for a run. The run id
// is a uuid so it needs no sanitizing; determinism is what lets the reconciler
// pair a crash-orphaned directory with its run.
func (l Layout) WorktreePath(runID string) string {
	return filepath.Join(l.WorktreeRoot, runID)
}

// QuarantinePath returns the quarantine destination for a worktree path.
func (l Layout) QuarantinePath(runID string) string {
	return filepath.Join(l.QuarantineRoot, runID)
}

// repoCacheKey is a collision-free, filesystem-safe directory name for a repo
// URL: readable host+path prefix plus a hash suffix.
func repoCacheKey(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	suffix := hex.EncodeToString(sum[:6])
	prefix := "repo"
	if u, err := url.Parse(repoURL); err == nil && u.Host != "" {
		cleaned := strings.Trim(u.Path, "/")
		cleaned = strings.TrimSuffix(cleaned, ".git")
		parts := append([]string{u.Host}, strings.Split(cleaned, "/")...)
		var b strings.Builder
		for _, p := range parts {
			for _, r := range p {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
					b.WriteRune(r)
				}
			}
			b.WriteByte('-')
		}
		if s := strings.Trim(b.String(), "-"); s != "" {
			prefix = s
		}
	}
	if len(prefix) > 64 {
		prefix = prefix[:64]
	}
	return prefix + "-" + suffix
}

// Git shells out to the git binary. Runner is injectable for tests.
type Git struct {
	// Run executes git. Nil defaults to os/exec.
	Runner func(ctx context.Context, dir string, args ...string) (string, error)
}

func (g Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	if g.Runner != nil {
		return g.Runner(ctx, dir, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// EnsureRepoCache clones repoURL into the cache once and fetches on later
// calls. The clone is a regular (non-bare) clone so `git worktree add` works.
func (g Git) EnsureRepoCache(ctx context.Context, layout Layout, repoURL string) (string, error) {
	if err := os.MkdirAll(layout.RepoCacheRoot, 0o755); err != nil {
		return "", fmt.Errorf("create repo cache root: %w", err)
	}
	dir := filepath.Join(layout.RepoCacheRoot, repoCacheKey(repoURL))
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if _, err := g.run(ctx, dir, "fetch", "--all", "--prune"); err != nil {
			return "", fmt.Errorf("fetch repo cache: %w", err)
		}
		return dir, nil
	}
	if _, err := g.run(ctx, layout.RepoCacheRoot, "clone", repoURL, dir); err != nil {
		return "", fmt.Errorf("clone repo: %w", err)
	}
	return dir, nil
}

// AddWorktree creates <path> on a new or existing branch tracking baseRef.
// Idempotent at the git level: a path that already contains a registered
// worktree on the same branch is treated as success (retry after crash).
func (g Git) AddWorktree(ctx context.Context, repoDir, path, branch, baseRef string) error {
	if _, err := g.run(ctx, repoDir, "rev-parse", "--verify", branch); err == nil {
		// Branch exists (previous attempt made it this far): attach it.
		if _, err := g.run(ctx, repoDir, "worktree", "add", path, branch); err != nil {
			if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
				return nil // path already holds a worktree: retry wins
			}
			return fmt.Errorf("worktree add existing branch: %w", err)
		}
		return nil
	}
	if _, err := g.run(ctx, repoDir, "worktree", "add", "-b", branch, path, baseRef); err != nil {
		if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
			return nil
		}
		return fmt.Errorf("worktree add: %w", err)
	}
	return nil
}

// HeadSHA resolves the worktree's base commit for the audit record.
func (g Git) HeadSHA(ctx context.Context, path string) (string, error) {
	return g.run(ctx, path, "rev-parse", "HEAD")
}

// IsDirty reports whether the worktree has ANY uncommitted state — tracked
// modifications, staged changes, or untracked files (advise decision 3: dirt
// of any kind means quarantine, never delete).
func (g Git) IsDirty(ctx context.Context, path string) (bool, error) {
	out, err := g.run(ctx, path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// RemoveWorktree removes a CLEAN worktree. Callers must check IsDirty first;
// --force is never used (policy: no destructive git cleanup).
func (g Git) RemoveWorktree(ctx context.Context, repoDir, path string) error {
	if _, err := g.run(ctx, repoDir, "worktree", "remove", path); err != nil {
		return fmt.Errorf("worktree remove: %w", err)
	}
	return nil
}

// Quarantine moves a worktree aside instead of deleting it. The dir is
// preserved first (rename onto the quarantine root); the repo's worktree
// metadata is then re-pointed via `git worktree repair`, or unregistered via
// `git worktree prune` when repair is unavailable — either way the common
// checkout metadata never references the old path again.
func (g Git) Quarantine(ctx context.Context, layout Layout, runID, path, reason string) (string, error) {
	if err := os.MkdirAll(layout.QuarantineRoot, 0o755); err != nil {
		return "", fmt.Errorf("create quarantine root: %w", err)
	}
	dest := layout.QuarantinePath(runID)
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("quarantine destination already exists: %s", dest)
	}
	// repoDir is "" for dirs that are not git worktrees (crash-window
	// orphans): a plain rename is all they need.
	repoDir, repoErr := worktreeMainRepo(path)
	if err := os.Rename(path, dest); err != nil {
		return "", fmt.Errorf("move worktree to quarantine: %w", err)
	}
	if repoErr == nil {
		if _, err := g.run(ctx, repoDir, "worktree", "repair", dest); err != nil {
			if _, perr := g.run(ctx, repoDir, "worktree", "prune"); perr != nil {
				return "", fmt.Errorf("quarantined to %s but git metadata still references %s: repair: %v; prune: %w", dest, path, err, perr)
			}
		}
	}
	return dest, nil
}

// worktreeMainRepo finds a linked worktree's main repo by reading its .git
// pointer file ("gitdir: <repo>/.git/worktrees/<name>").
func worktreeMainRepo(path string) (string, error) {
	b, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir:") {
		return "", errors.New("not a worktree .git pointer file")
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	admin := filepath.Dir(gitdir)
	gitDir := filepath.Dir(admin)
	if filepath.Base(admin) != "worktrees" || filepath.Base(gitDir) != ".git" {
		return "", fmt.Errorf("unexpected worktree gitdir %s", gitdir)
	}
	return filepath.Dir(gitDir), nil
}

var ErrUnsafePath = errors.New("gitops: path escapes configured root")

// CanonicalPath resolves path and proves it stays under root. Neither the
// root nor the leaf need to exist yet (fresh deploys, allocation targets):
// resolution walks to the deepest existing ancestor and re-appends the
// not-yet-existing suffix. An existing leaf that is itself a symlink is
// rejected outright — ancestor checks alone would never inspect it. This is
// the symlink/traversal gate the spec requires before any fs or git mutation.
func CanonicalPath(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if absPath != absRoot && !strings.HasPrefix(absPath, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s not under %s", ErrUnsafePath, absPath, absRoot)
	}
	if fi, err := os.Lstat(absPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s is a symlink", ErrUnsafePath, absPath)
	}
	resolved, err := resolveExistingPrefix(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve path ancestor: %w", err)
	}
	resolvedRoot, err := resolveExistingPrefix(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s resolves outside %s", ErrUnsafePath, absPath, absRoot)
	}
	return absPath, nil
}

// resolveExistingPrefix EvalSymlinks the deepest existing ancestor of path
// (every existing component is inspected) and re-appends the non-existent
// suffix, so roots and leaves that do not exist yet still get checked.
func resolveExistingPrefix(path string) (string, error) {
	probe := path
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("no existing ancestor for %s", path)
		}
		probe = parent
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	return resolved + path[len(probe):], nil
}

// ListWorktreeDirs returns child directory names under WorktreeRoot that look
// like allocation targets (any directory — the caller matches them to rows).
func ListWorktreeDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
