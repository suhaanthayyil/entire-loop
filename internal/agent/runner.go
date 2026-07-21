package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// MaxOutputBytes caps a worker's captured stdout (the JSON envelope).
	MaxOutputBytes = 256 * 1024
	// MaxStderrBytes caps a worker's captured stderr.
	MaxStderrBytes = 256 * 1024
	// DefaultWorkerTimeout is the per-worker wall-clock budget.
	DefaultWorkerTimeout = 1200 * time.Second
)

// runResultMeta carries out-of-band details about a reaped run that do not fit
// the positional (stdout, stderr, timedOut, err) return of runWithReaping:
// whether the captured stdout/stderr hit their caps (were truncated), and whether
// the process group could NOT be reaped after SIGKILL (a D-state child that was
// abandoned to a detached waiter). It is populated through a variadic pointer
// out-param so the 4-value return signature — which three tests and spawn_other.go
// destructure positionally — is preserved.
type runResultMeta struct {
	stdoutTruncated bool
	stderrTruncated bool
	reapFailed      bool
}

// Runner runs one worker seat and returns its parsed result. Implementations
// never return an error: a failed worker degrades to a not-OK SeatResult with
// warnings so the round continues. The loop depends on this interface so tests
// can substitute a fake and never spawn a real `claude`.
type Runner interface {
	Run(ctx context.Context, spec SeatSpec, brief string) SeatResult
}

// MutatingBuildRunner is a Runner that can execute the build seat inside an
// isolated throwaway clone the CALLER prepared, capturing its diff. The loop
// creates the clone (via agent.NewBuildClone) so it can bind the worker's brief to
// the clone dir — the bypass worker is thus never handed the real repo path.
// ExecRunner implements it; a test FakeRunner does not, so tests exercise the
// non-isolated Run path.
type MutatingBuildRunner interface {
	Runner
	RunBypassInClone(ctx context.Context, spec SeatSpec, brief string, clone *BuildClone) SeatResult
}

// ExecRunner is the production Runner: it builds the worker argv, gates egress,
// spawns Claude Code with full process-group reaping, and parses the result. It
// is safe for concurrent use (each call is independent); the loop bounds
// concurrency with a semaphore.
type ExecRunner struct {
	// RepoRoot is the resolved repository root, exported into deep seats' child
	// env and used as the default when a SeatSpec carries no RepoRoot.
	RepoRoot string
	// Dir is the working directory for a NON-mutating worker process (defaults to
	// RepoRoot). Mutating seats ignore it — they run in a throwaway worktree.
	Dir string
	// Timeout is the per-worker wall-clock budget (defaults to DefaultWorkerTimeout).
	Timeout time.Duration
	// RunDir, when set, enables the round-scoped idempotent skip: a completed seat's
	// OK result is cached at <RunDir>/seats/round-<N>/<role>.json and reused instead
	// of re-running. It also roots the build worktrees.
	RunDir string
}

// Run executes one seat. It honors no-egress, the round-scoped idempotent skip
// cache, the per-worker timeout, and process-group reaping. A mutating seat runs
// in a throwaway worktree; every other seat runs plan-mode read-only.
func (r ExecRunner) Run(ctx context.Context, spec SeatSpec, brief string) SeatResult {
	if err := RejectForNoEgress("claude"); err != nil {
		return SeatResult{Role: spec.Role, OK: false, Warnings: []string{err.Error()}}
	}

	// Idempotent skip: a completed OK seat output in the run dir is a completion
	// marker; reuse it rather than re-running the worker. The cache is round-scoped
	// so round N never replays round N-1.
	if cached, ok := r.loadCached(spec); ok {
		return cached
	}

	if spec.RepoRoot == "" {
		spec.RepoRoot = r.RepoRoot
	}
	// Degrade (never silently bind the ambient cwd): a deep seat with no repo root
	// means the brain would bind the orchestrator's directory. Warn and continue.
	degradeWarn := repoBindWarning(spec)
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultWorkerTimeout
	}

	var result SeatResult
	if spec.Mutating {
		result = r.runMutating(ctx, spec, brief, timeout)
	} else {
		result = r.runPlan(ctx, spec, brief, timeout)
	}
	if degradeWarn != "" {
		result.Warnings = append(result.Warnings, degradeWarn)
	}

	r.saveCached(spec, result)
	return result
}

// runPlan runs a non-mutating (plan-mode) worker in its bound directory.
func (r ExecRunner) runPlan(ctx context.Context, spec SeatSpec, brief string, timeout time.Duration) SeatResult {
	cmd := BuildCmd(ctx, spec, brief)
	// BuildCmd already binds Dir to spec.RepoRoot; an explicit runner Dir wins.
	if r.Dir != "" {
		cmd.Dir = r.Dir
	}
	var meta runResultMeta
	stdout, stderr, timedOut, err := runWithReaping(ctx, cmd, timeout, &meta)
	return finalizeResult(spec, stdout, stderr, timedOut, err, meta, timeout)
}

// runMutating runs the build seat inside a confined, throwaway CLONE so it can
// write real code, then captures the diff as the proposal. A clone (not a
// `git worktree`) has its own object store + refs, so nothing the worker does can
// reach the user's repo. If the target is not a git repo, or the clone fails, it
// degrades to the non-mutating propose-as-text path with a warning. This is the
// self-contained direct-Run path; the loop's primary path builds the clone first
// so the brief can be bound to it (see RunBypassInClone) — this path reuses the
// caller-supplied brief.
func (r ExecRunner) runMutating(ctx context.Context, spec SeatSpec, brief string, timeout time.Duration) SeatResult {
	if !IsGitRepo(ctx, spec.RepoRoot) {
		res := r.runPlan(ctx, spec, brief, timeout)
		res.Warnings = append(res.Warnings,
			"mutating build unavailable: target is not a git repo; fell back to non-mutating propose-as-text")
		return res
	}

	res, err := buildInClone(ctx, spec.RepoRoot, r.bypassExec(spec, brief, timeout))
	if err != nil {
		res := r.runPlan(ctx, spec, brief, timeout)
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("mutating build degraded (%v); fell back to non-mutating propose-as-text", err))
		return res
	}
	return res
}

// RunBypassInClone runs the bypassPermissions build worker inside an ALREADY
// prepared throwaway clone and captures its base→worktree diff as the proposal.
// The loop creates the clone itself (via agent.NewBuildClone) so it can bind the
// worker's brief to the clone dir — the bypass worker is therefore never handed
// the real repo path in its cwd, env, brief, or the clone's git config. The
// caller owns the clone lifecycle (create + cleanup); this only runs + captures.
// It honors the no-egress gate and the round-scoped idempotent cache.
func (r ExecRunner) RunBypassInClone(ctx context.Context, spec SeatSpec, brief string, clone *BuildClone) SeatResult {
	if err := RejectForNoEgress("claude"); err != nil {
		return SeatResult{Role: spec.Role, OK: false, Warnings: []string{err.Error()}}
	}
	if cached, ok := r.loadCached(spec); ok {
		return cached
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultWorkerTimeout
	}
	res := captureBuild(ctx, r.bypassExec(spec, brief, timeout), clone)
	r.saveCached(spec, res)
	return res
}

// bypassExec returns the worker closure that spawns claude in bypassPermissions
// mode in a build dir, with process-group reaping, and parses the result.
func (r ExecRunner) bypassExec(spec SeatSpec, brief string, timeout time.Duration) worktreeExec {
	return func(ctx context.Context, dir string) SeatResult {
		cmd := BuildMutatingCmd(ctx, spec, brief, dir)
		var meta runResultMeta
		stdout, stderr, timedOut, cErr := runWithReaping(ctx, cmd, timeout, &meta)
		return finalizeResult(spec, stdout, stderr, timedOut, cErr, meta, timeout)
	}
}

// finalizeResult turns the raw output of a reaped worker into a SeatResult: parse
// the envelope, then fold in the timeout/failure/truncation/reap warnings.
func finalizeResult(spec SeatSpec, stdout, stderr string, timedOut bool, err error, meta runResultMeta, timeout time.Duration) SeatResult {
	result := ParseSeatOutput(spec.Role, stdout)
	switch {
	case timedOut:
		result.OK = false
		result.Warnings = append(result.Warnings, fmt.Sprintf("worker timed out after %s", timeout))
	case err != nil:
		result.OK = false
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("worker failed: %v: %s", err, truncateWarning(strings.TrimSpace(stderr))))
	}
	if meta.stdoutTruncated {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("worker stdout truncated at %d bytes; parsed output may be incomplete", MaxOutputBytes))
	}
	if meta.reapFailed {
		result.OK = false
		result.Warnings = append(result.Warnings,
			"process could not be reaped after SIGKILL; the worker child may still be running detached")
	}
	return result
}

// loadCached returns a previously persisted OK seat result if the round-scoped run
// dir holds one. A non-OK cached result is IGNORED: a transient failure must never
// poison the seat for the rest of the run.
func (r ExecRunner) loadCached(spec SeatSpec) (SeatResult, bool) {
	if r.RunDir == "" {
		return SeatResult{}, false
	}
	data, err := os.ReadFile(r.seatPath(spec.Round, spec.Role))
	if err != nil {
		return SeatResult{}, false
	}
	var res SeatResult
	if err := json.Unmarshal(data, &res); err != nil {
		return SeatResult{}, false
	}
	if !res.OK {
		return SeatResult{}, false
	}
	res.Warnings = append(res.Warnings, "reused cached seat output (idempotent skip)")
	return res, true
}

// saveCached persists a COMPLETED, OK seat result as its round-scoped completion
// marker. A failed or timed-out result is never cached, so a transient failure
// does not poison a re-run. The write is atomic (temp file + rename, mirroring
// state.Store.Save) so a crash mid-write can never leave a truncated cache. The
// <round>/<role>.json path is unique per round+role (the planner enforces one seat
// per role per round), so no two writers race the same path.
func (r ExecRunner) saveCached(spec SeatSpec, res SeatResult) {
	if r.RunDir == "" || !res.OK {
		return
	}
	dir := filepath.Dir(r.seatPath(spec.Round, res.Role))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, res.Role+"-*.json.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, r.seatPath(spec.Round, res.Role)); err != nil {
		_ = os.Remove(tmpName)
	}
}

// seatPath is the round-scoped completion-marker location for a seat.
func (r ExecRunner) seatPath(round int, role string) string {
	return filepath.Join(r.RunDir, "seats", fmt.Sprintf("round-%d", round), role+".json")
}

// cappedBuffer is a bytes.Buffer that stops storing after limit bytes and
// records that it overflowed. It never errors on Write. The exceeded flag is read
// back out of runWithReaping via runResultMeta and surfaced as a seat warning.
type cappedBuffer struct {
	limit    int
	exceeded bool
	buf      bytes.Buffer
}

func newCappedBuffer(limit int) cappedBuffer { return cappedBuffer{limit: limit} }

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.exceeded = true
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = w.buf.Write(p)
			return len(p), nil
		}
		_, _ = w.buf.Write(p[:remaining])
	}
	w.exceeded = true
	return len(p), nil
}

func (w *cappedBuffer) String() string { return w.buf.String() }

func truncateWarning(warning string) string {
	const maxWarningBytes = 4000
	if len(warning) <= maxWarningBytes {
		return warning
	}
	return warning[:maxWarningBytes] + "\n[truncated]"
}
