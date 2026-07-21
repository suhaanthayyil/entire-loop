package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitOpTimeout bounds each git plumbing call so a wedged git can never stall the
// mutating-build path. Clone/diff are fast; this is a safety valve.
const gitOpTimeout = 120 * time.Second

// buildCloneTempPrefix is the OS-temp basename prefix for a throwaway build clone.
// Orphan reclamation matches on it, so it must be stable.
const buildCloneTempPrefix = "entire-loop-build-"

// orphanCloneMaxAge is how old a leftover build clone must be before startup
// reclamation removes it. Age-gating avoids racing a clone a concurrent run is
// actively using — only demonstrably stale dirs are swept.
const orphanCloneMaxAge = 2 * time.Hour

// worktreeExec runs a seat's worker inside the given build dir and returns its
// SeatResult. In production it spawns claude in bypassPermissions mode; tests
// substitute a fake that writes files into dir. Decoupling the worker from the
// clone lifecycle is what lets the mutating-build logic be tested with a fake
// runner and a real temp git repo — no `claude` process.
type worktreeExec func(ctx context.Context, dir string) SeatResult

// BuildClone is a throwaway LOCAL clone of a target repo used by the mutating
// build seat. Unlike a `git worktree`, a clone has its OWN object store and refs,
// so ref/object/tag/branch mutations inside it can NEVER reach the user's repo.
// It lives in the OS temp dir (never under the repo) and carries no reference to
// the real repo (the origin remote is removed), so a bypass worker cannot learn
// or write to the real path. Dir is the clone path; BaseSha is HEAD at clone
// time — the diff base, so a worker that COMMITS its work still yields a diff.
type BuildClone struct {
	Dir     string
	BaseSha string
}

// IsGitRepo reports whether root is inside a git working tree.
func IsGitRepo(ctx context.Context, root string) bool {
	if root == "" {
		return false
	}
	_, err := runGit(ctx, root, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// NewBuildClone creates a confined, throwaway clone of repoRoot for a mutating
// build worker and returns it with a cleanup func. Guarantees:
//   - The clone lives in the OS temp dir (os.MkdirTemp), NEVER under repoRoot and
//     NEVER under a plugin data dir, so the worker's writes are fully off-repo.
//   - `git clone --local --no-hardlinks` gives the clone its OWN object store and
//     refs; --no-hardlinks means even the object files are copies, so nothing the
//     worker does to objects/refs/tags/branches can reach the user's repo.
//   - The `origin` remote (which git points at repoRoot's absolute path) is
//     removed, so the clone carries no reference to the real repo path.
//
// The returned cleanup removes the clone; callers must call it on all normal,
// panic, and cancel paths. It is context-free (plain RemoveAll) so a cancelled
// round still tears the clone down.
func NewBuildClone(ctx context.Context, repoRoot string) (*BuildClone, func(), error) {
	if !IsGitRepo(ctx, repoRoot) {
		return nil, func() {}, fmt.Errorf("not a git repo: %s", repoRoot)
	}
	baseRaw, err := runGit(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, func() {}, fmt.Errorf("resolve base commit: %w", err)
	}
	baseSha := strings.TrimSpace(string(baseRaw))

	dst, err := os.MkdirTemp("", buildCloneTempPrefix+"*")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dst) }

	// Clone at the resolved base into the (existing, empty) temp dir. --local +
	// --no-hardlinks copy objects so the clone is fully independent of the source.
	if _, err := runGit(ctx, repoRoot, "clone", "--local", "--no-hardlinks", repoRoot, dst); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("clone: %w", err)
	}
	// Strip the origin remote so the clone holds no pointer to the real repo path.
	// Best-effort: a clone with no origin is still fine to build in.
	_, _ = runGit(ctx, dst, "remote", "remove", "origin")

	return &BuildClone{Dir: dst, BaseSha: baseSha}, cleanup, nil
}

// CaptureCloneDiff stages every change in the clone and returns the diff against
// the base commit resolved at clone time. Diffing against baseSha (not
// `--cached`) means a worker that COMMITS its work still yields a non-empty
// proposal: the diff is base→working-tree, covering committed, staged, and
// unstaged changes alike. add -A only touches the throwaway clone's index.
func CaptureCloneDiff(ctx context.Context, dir, baseSha string) (string, error) {
	if _, err := runGit(ctx, dir, "add", "-A"); err != nil {
		return "", err
	}
	out, err := runGit(ctx, dir, "diff", "--no-color", baseSha)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// captureBuild runs exec in clone.Dir and folds the captured base→worktree diff
// into the result as the authoritative proposal (what the worker actually wrote,
// not what it claimed in its JSON). It does NOT create or remove the clone — the
// caller owns the clone lifecycle.
func captureBuild(ctx context.Context, exec worktreeExec, clone *BuildClone) SeatResult {
	res := exec(ctx, clone.Dir)

	diff, err := CaptureCloneDiff(ctx, clone.Dir, clone.BaseSha)
	switch {
	case err != nil:
		res.Warnings = append(res.Warnings, fmt.Sprintf("could not capture clone diff: %v", err))
	case strings.TrimSpace(diff) == "":
		res.Warnings = append(res.Warnings, "mutating build produced no file changes; proposal is empty")
	default:
		res.Proposal = diff
	}
	return res
}

// buildInClone creates a throwaway clone of repoRoot, runs exec (the bypass
// worker, or a fake in tests) inside it, captures the diff, and removes the clone
// on all paths. It returns an error only when the clone cannot be created, so the
// caller can degrade to the non-mutating propose-as-text path. The user's real
// repo — its working tree, refs, tags, and objects — is never touched.
func buildInClone(ctx context.Context, repoRoot string, exec worktreeExec) (SeatResult, error) {
	clone, cleanup, err := NewBuildClone(ctx, repoRoot)
	if err != nil {
		return SeatResult{}, err
	}
	// Detached teardown: a cancelled round still removes the clone rather than
	// leaking it (cleanup is context-free RemoveAll).
	defer cleanup()
	return captureBuild(ctx, exec, clone), nil
}

// PruneOrphanBuildClones reclaims stale throwaway build clones left by a crashed
// or killed run: it removes OS-temp dirs matching the build-clone prefix that are
// older than orphanCloneMaxAge. Age-gating avoids deleting a clone a concurrent
// run is actively using. It is best-effort — errors are swallowed — and returns
// the number of dirs removed for logging.
func PruneOrphanBuildClones() int {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-orphanCloneMaxAge)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), buildCloneTempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.RemoveAll(filepath.Join(tmp, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// runGit runs `git -C dir <args...>` under a bounded timeout and returns combined
// output on error for a useful message.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	tctx, cancel := context.WithTimeout(ctx, gitOpTimeout)
	defer cancel()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(tctx, "git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
