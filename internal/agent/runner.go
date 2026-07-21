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

// Runner runs one worker seat and returns its parsed result. Implementations
// never return an error: a failed worker degrades to a not-OK SeatResult with
// warnings so the round continues. The loop depends on this interface so tests
// can substitute a fake and never spawn a real `claude`.
type Runner interface {
	Run(ctx context.Context, spec SeatSpec, brief string) SeatResult
}

// ExecRunner is the production Runner: it builds the worker argv, gates egress,
// spawns Claude Code with full process-group reaping, and parses the result. It
// is safe for concurrent use (each call is independent); the loop bounds
// concurrency with a semaphore.
type ExecRunner struct {
	// RepoRoot is the resolved repository root, exported into deep seats' child
	// env and used as the default when a SeatSpec carries no RepoRoot.
	RepoRoot string
	// Dir is the working directory for the worker process (defaults to RepoRoot).
	Dir string
	// Timeout is the per-worker wall-clock budget (defaults to DefaultWorkerTimeout).
	Timeout time.Duration
	// RunDir, when set, enables idempotent skip: a completed seat's result is
	// cached at <RunDir>/seats/<role>.json and reused instead of re-running.
	RunDir string
}

// Run executes one seat. It honors no-egress, the idempotent skip cache, the
// per-worker timeout, and process-group reaping.
func (r ExecRunner) Run(ctx context.Context, spec SeatSpec, brief string) SeatResult {
	if err := RejectForNoEgress("claude"); err != nil {
		return SeatResult{Role: spec.Role, OK: false, Warnings: []string{err.Error()}}
	}

	// Idempotent skip: a completed seat output in the run dir is a completion
	// marker; reuse it rather than re-running the worker.
	if cached, ok := r.loadCached(spec.Role); ok {
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

	cmd := BuildCmd(ctx, spec, brief)
	// BuildCmd already binds Dir to spec.RepoRoot; an explicit runner Dir wins.
	if r.Dir != "" {
		cmd.Dir = r.Dir
	}

	stdout, stderr, timedOut, err := runWithReaping(ctx, cmd, timeout)

	result := ParseSeatOutput(spec.Role, stdout)
	if degradeWarn != "" {
		result.Warnings = append(result.Warnings, degradeWarn)
	}
	switch {
	case timedOut:
		result.OK = false
		result.Warnings = append(result.Warnings, fmt.Sprintf("worker timed out after %s", timeout))
	case err != nil:
		result.OK = false
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("worker failed: %v: %s", err, truncateWarning(strings.TrimSpace(stderr))))
	}

	r.saveCached(result)
	return result
}

// loadCached returns a previously persisted seat result if the run dir holds one.
func (r ExecRunner) loadCached(role string) (SeatResult, bool) {
	if r.RunDir == "" {
		return SeatResult{}, false
	}
	data, err := os.ReadFile(r.seatPath(role))
	if err != nil {
		return SeatResult{}, false
	}
	var res SeatResult
	if err := json.Unmarshal(data, &res); err != nil {
		return SeatResult{}, false
	}
	res.Warnings = append(res.Warnings, "reused cached seat output (idempotent skip)")
	return res, true
}

// saveCached persists a completed seat result as its completion marker. The
// write is atomic (temp file + rename, mirroring state.Store.Save) so a crash
// mid-write can never leave a truncated, corrupt cache that a later idempotent
// skip would then reuse. Uniqueness of the <role>.json path is guaranteed by the
// planner (one seat per role per round), so no two writers race the same path.
func (r ExecRunner) saveCached(res SeatResult) {
	if r.RunDir == "" {
		return
	}
	dir := filepath.Join(r.RunDir, "seats")
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
	if err := os.Rename(tmpName, r.seatPath(res.Role)); err != nil {
		_ = os.Remove(tmpName)
	}
}

func (r ExecRunner) seatPath(role string) string {
	return filepath.Join(r.RunDir, "seats", role+".json")
}

// cappedBuffer is a bytes.Buffer that stops storing after limit bytes and
// records that it overflowed. It never errors on Write.
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
