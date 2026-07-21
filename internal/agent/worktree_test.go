package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitEnv is the deterministic author/committer identity for test commits.
var gitEnv = []string{
	"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
	"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
}

// initGitRepo creates a throwaway git repo with one committed file so HEAD exists,
// and returns its root. It skips the test if git is unavailable.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "seed")
	return root
}

// gitIn runs a git command in dir with the deterministic identity and fails the
// test on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestBuildInClone_IsolatesAndCapturesDiff exercises the clone lifecycle with a
// FAKE worker that writes a real file into the clone (never spawning claude): the
// new file must appear in the captured proposal diff; the clone must be removed
// afterward; the target repo's main working tree must be untouched; and the dir
// the worker ran in must be the confined OS-temp clone (never the real repoRoot).
func TestBuildInClone_IsolatesAndCapturesDiff(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)

	var sawDir string
	fake := func(_ context.Context, dir string) SeatResult {
		sawDir = dir
		if err := os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package x\n"), 0o600); err != nil {
			t.Fatalf("fake write: %v", err)
		}
		return SeatResult{Role: RoleBuild, OK: true, Findings: []string{"added feature.go"}, Proposal: "claimed-but-ignored"}
	}

	res, err := buildInClone(context.Background(), repo, fake)
	if err != nil {
		t.Fatalf("buildInClone: %v", err)
	}

	// The worker ran in the confined clone, NOT the real repo path.
	if sawDir == repo {
		t.Fatalf("worker ran in the real repoRoot %q — must run in the throwaway clone", repo)
	}
	if strings.HasPrefix(sawDir, repo) {
		t.Errorf("clone %q must not live under the target repo %q", sawDir, repo)
	}
	if !strings.HasPrefix(sawDir, os.TempDir()) {
		t.Errorf("clone %q must live under the OS temp dir %q", sawDir, os.TempDir())
	}
	if !strings.HasPrefix(filepath.Base(sawDir), buildCloneTempPrefix) {
		t.Errorf("clone leaf %q must carry the %q prefix", filepath.Base(sawDir), buildCloneTempPrefix)
	}

	// The captured diff (not the worker's claimed string) is the proposal.
	if !strings.Contains(res.Proposal, "feature.go") || !strings.Contains(res.Proposal, "package x") {
		t.Errorf("proposal should be the captured clone diff; got:\n%s", res.Proposal)
	}
	if res.Proposal == "claimed-but-ignored" {
		t.Errorf("proposal should be the captured diff, not the worker's claim")
	}
	// The clone was torn down.
	if _, statErr := os.Stat(sawDir); !os.IsNotExist(statErr) {
		t.Errorf("clone %q should have been removed; stat err = %v", sawDir, statErr)
	}
	// The target repo's main working tree is untouched.
	if _, statErr := os.Stat(filepath.Join(repo, "feature.go")); !os.IsNotExist(statErr) {
		t.Errorf("target repo main tree must NOT contain the clone's new file")
	}
}

// TestBuildInClone_CapturesCommittedChange is the diff-vs-base guard: a worker
// that COMMITS its work still yields a non-empty proposal, because the diff is
// taken against the base commit resolved at clone time (not `--cached`).
func TestBuildInClone_CapturesCommittedChange(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)

	fake := func(_ context.Context, dir string) SeatResult {
		if err := os.WriteFile(filepath.Join(dir, "committed.go"), []byte("package committed\n"), 0o600); err != nil {
			t.Fatalf("fake write: %v", err)
		}
		// The worker commits its work — a `git diff --cached` would be EMPTY here.
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "-q", "-m", "worker work")
		return SeatResult{Role: RoleBuild, OK: true}
	}

	res, err := buildInClone(context.Background(), repo, fake)
	if err != nil {
		t.Fatalf("buildInClone: %v", err)
	}
	if !strings.Contains(res.Proposal, "committed.go") || !strings.Contains(res.Proposal, "package committed") {
		t.Errorf("a COMMITTED change must still be captured via diff-vs-base; got:\n%s", res.Proposal)
	}
	if hasWarning(res.Warnings, "no file changes") {
		t.Errorf("a committed change must not read as an empty proposal; warnings=%v", res.Warnings)
	}
}

// TestNewBuildClone_TargetRefsAndTreeUnchanged is the core isolation guard: even a
// worker that mutates refs/tags/branches and commits inside the clone cannot reach
// the user's repo, because a clone has its own object store and refs. It also
// asserts the clone carries no `origin` remote (no reference to the real path).
func TestNewBuildClone_TargetRefsAndTreeUnchanged(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	// Plant a tag and a branch on the target so we can prove they survive.
	gitIn(t, repo, "tag", "planted-tag")
	gitIn(t, repo, "branch", "planted-branch")
	wantHead := gitIn(t, repo, "rev-parse", "HEAD")
	wantBranch := gitIn(t, repo, "rev-parse", "planted-branch")

	clone, cleanup, err := NewBuildClone(context.Background(), repo)
	if err != nil {
		t.Fatalf("NewBuildClone: %v", err)
	}
	defer cleanup()

	// The clone must not point back at the real repo (origin removed).
	if remotes := gitIn(t, clone.Dir, "remote"); remotes != "" {
		t.Errorf("clone must have no origin remote (no real-path leak); got %q", remotes)
	}
	if clone.BaseSha != wantHead {
		t.Errorf("clone BaseSha = %q, want target HEAD %q", clone.BaseSha, wantHead)
	}

	// A hostile worker mutates refs, tags, branches, and the tree in the CLONE.
	if err := os.WriteFile(filepath.Join(clone.Dir, "evil.txt"), []byte("evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone.Dir, "add", "-A")
	gitIn(t, clone.Dir, "commit", "-q", "-m", "evil")
	gitIn(t, clone.Dir, "tag", "-f", "planted-tag")
	gitIn(t, clone.Dir, "branch", "-f", "planted-branch", "HEAD")
	gitIn(t, clone.Dir, "update-ref", "refs/heads/attacker", "HEAD")

	// The target repo is completely unchanged: HEAD, the planted branch, its tags,
	// and its working tree.
	if got := gitIn(t, repo, "rev-parse", "HEAD"); got != wantHead {
		t.Errorf("target HEAD moved: %q != %q", got, wantHead)
	}
	if got := gitIn(t, repo, "rev-parse", "planted-branch"); got != wantBranch {
		t.Errorf("target planted-branch moved: %q != %q", got, wantBranch)
	}
	if tags := gitIn(t, repo, "tag"); tags != "planted-tag" {
		t.Errorf("target tags changed: %q, want just planted-tag", tags)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "evil.txt")); !os.IsNotExist(statErr) {
		t.Errorf("target working tree must NOT contain the clone's evil.txt")
	}
}

// TestBuildInClone_NotARepoDegrades: when the target is not a git repo, the clone
// cannot be created, so buildInClone returns an error (the caller degrades to the
// non-mutating path) and the worker never runs.
func TestBuildInClone_NotARepoDegrades(t *testing.T) {
	t.Parallel()
	notARepo := t.TempDir()
	_, err := buildInClone(context.Background(), notARepo, func(context.Context, string) SeatResult {
		t.Error("worker must not run when the clone cannot be created")
		return SeatResult{}
	})
	if err == nil {
		t.Errorf("expected an error when the clone cannot be created")
	}
}

func TestIsGitRepo(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	if !IsGitRepo(context.Background(), repo) {
		t.Errorf("initialized repo should be a git repo")
	}
	if IsGitRepo(context.Background(), t.TempDir()) {
		t.Errorf("a bare temp dir is not a git repo")
	}
	if IsGitRepo(context.Background(), "") {
		t.Errorf("empty root is not a git repo")
	}
}

// TestPruneOrphanBuildClones removes STALE leftover build clones and leaves fresh
// ones (which a concurrent run may be using) alone.
func TestPruneOrphanBuildClones(t *testing.T) {
	// No t.Parallel: this scans and mutates the shared OS temp dir.
	tmp := os.TempDir()

	stale, err := os.MkdirTemp(tmp, buildCloneTempPrefix+"stale-*")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := os.MkdirTemp(tmp, buildCloneTempPrefix+"fresh-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(fresh)
	// Also plant an unrelated temp dir that must never be touched.
	unrelated, err := os.MkdirTemp(tmp, "some-other-tool-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(unrelated)

	// Backdate the stale clone well past the reclamation age.
	old := time.Now().Add(-orphanCloneMaxAge - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	PruneOrphanBuildClones()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale build clone should have been reclaimed; stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh build clone must NOT be reclaimed; stat err = %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("an unrelated temp dir must never be touched; stat err = %v", err)
	}
}
