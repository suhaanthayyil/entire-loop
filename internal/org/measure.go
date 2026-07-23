package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
)

// defaultMeasureTimeout bounds a single external measure command so a wedged
// measurement can never stall a round.
const defaultMeasureTimeout = 120 * time.Second

// defaultMeasureMaxOutput caps how much stdout the default executor buffers from a
// measure command. A chatty or wedged command (`cat /dev/zero`) that blew past
// this would OOM the loop, so output over the cap is treated as a measure FAILURE
// (the round degrades to its claude-derived metrics) rather than read unbounded.
const defaultMeasureMaxOutput = 4 << 20 // 4 MiB

// measureWaitGrace bounds how long Wait lingers after the measure command's
// context is cancelled (and its process group killed) so a forking grandchild that
// still holds a pipe open can never keep Wait — and thus the round — from returning.
const measureWaitGrace = 2 * time.Second

// RoundMetrics is the typed view of a round's numeric signals. Progress and Risk
// are first-class (the loop reasons over them — the budget>progress reorg edge and
// external convergence — so they get named accessors rather than stringly-typed
// map lookups); any other keys ride along in the underlying map. It round-trips to
// the persisted map[string]float64 without loss via ToMap, so the on-disk schema
// and every existing map-based test are unchanged.
type RoundMetrics struct {
	values map[string]float64
}

// metricsFrom wraps a raw metric map as typed RoundMetrics (nil-safe).
func metricsFrom(values map[string]float64) RoundMetrics {
	if values == nil {
		values = map[string]float64{}
	}
	return RoundMetrics{values: values}
}

// Progress is the first-class progress signal (0 when absent).
func (m RoundMetrics) Progress() float64 { return m.values["progress"] }

// Risk is the first-class risk signal (0 when absent).
func (m RoundMetrics) Risk() float64 { return m.values["risk"] }

// Get returns an arbitrary metric and whether it was present.
func (m RoundMetrics) Get(key string) (float64, bool) {
	v, ok := m.values[key]
	return v, ok
}

// Len reports how many metrics were captured.
func (m RoundMetrics) Len() int { return len(m.values) }

// ToMap returns an independent copy of the underlying metric map for persistence.
func (m RoundMetrics) ToMap() map[string]float64 {
	out := make(map[string]float64, len(m.values))
	for k, v := range m.values {
		out[k] = v
	}
	return out
}

// MeasureExec runs the external measure command and returns its raw stdout. It is
// injectable so a test can stub the command without spawning a shell.
type MeasureExec func(ctx context.Context, cmd string) ([]byte, error)

// MeasureEdge is the external-signal edge (course taxonomy, NEW): it runs a
// user-configured command, bounds it with a timeout, and parses its JSON stdout
// into typed REAL round metrics (test count, coverage, benchmark) that override the
// claude-reasoned ones. Those metrics feed the runtime-reorg edge every round, and
// — only when the run configures a metric threshold (Options.ConvergeMetric) — can
// end the run by crossing it. By default they do NOT drive convergence (verdict /
// dry-streak still do); the edge measures, it does not on its own stop the loop.
//
// Guardrails: it is OFF by default (only runs when --measure-cmd is set — explicit
// user config), it is TIMEOUT-bounded (the command runs in its OWN process group
// and the whole group is killed on timeout, so a forking grandchild cannot outlive
// it), its stdout is capped (over-cap → treated as failure), its environment is
// SCRUBBED to the same credential allowlist as the mutating build seat, and it is
// meant to be NON-mutating (a read-only measurement such as a test/lint run). The
// loop never derives write privilege from it and never feeds it planner/agent
// output; it is the user's responsibility to point it at a read-only command.
type MeasureEdge struct {
	// Cmd is the shell command to run (via `sh -c`). Empty disables the edge.
	Cmd string
	// Dir is the working directory. The loop points this at the mutating-build CLONE
	// (which HAS the round's proposed change) when a mutating build ran, and at the
	// target repo root otherwise. Empty = current dir.
	Dir string
	// Timeout bounds one measurement (0 → defaultMeasureTimeout).
	Timeout time.Duration
	// MaxOutputBytes caps the default executor's buffered stdout (0 →
	// defaultMeasureMaxOutput). Ignored when Exec is supplied.
	MaxOutputBytes int
	// Exec runs the command; nil → the default `sh -c` executor bound to Dir.
	Exec MeasureExec
}

func (MeasureEdge) Name() string   { return "measure:external" }
func (MeasureEdge) Kind() EdgeKind { return KindMeasure }

// Enabled reports whether the edge is configured to run.
func (e MeasureEdge) Enabled() bool { return e.Cmd != "" }

// Measure runs the external command under a bounded timeout and parses its stdout
// as typed round metrics. Any failure (no command, exec error, unparseable output,
// no numeric metric) returns an error so the caller degrades gracefully — the loop
// keeps the round's claude-derived metrics and logs the failure.
func (e MeasureEdge) Measure(ctx context.Context) (RoundMetrics, error) {
	if e.Cmd == "" {
		return RoundMetrics{}, errors.New("measure edge: no command configured")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultMeasureTimeout
	}
	maxOut := e.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMeasureMaxOutput
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	execFn := e.Exec
	if execFn == nil {
		execFn = func(ctx context.Context, cmd string) ([]byte, error) {
			return runMeasureShell(ctx, cmd, e.Dir, maxOut)
		}
	}
	out, err := execFn(tctx, e.Cmd)
	if err != nil {
		return RoundMetrics{}, fmt.Errorf("measure command failed: %w", err)
	}
	return parseMeasureMetrics(out)
}

// runMeasureShell is the default measure executor. It runs `sh -c cmd` with three
// hardening properties the naive c.Output() lacked:
//   - process group + kill-the-group on timeout (Setpgid + Cancel, per-platform),
//     so a command that forks a grandchild holding stdout open (e.g.
//     `sleep 30 & echo queued`) is fully reaped when the context expires rather
//     than keeping Wait — and the whole round — blocked until the grandchild dies;
//   - a WaitDelay grace so Wait itself is bounded even if the kill is slow;
//   - a bounded read (io.LimitReader over a StdoutPipe), so a chatty/wedged command
//     cannot OOM the loop; output over the cap is a failure so the round degrades.
//
// The command's environment is scrubbed to the same credential allowlist as the
// mutating build seat, so a measure command never inherits ambient tokens/creds.
func runMeasureShell(ctx context.Context, cmd, dir string, maxOut int) ([]byte, error) {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = dir
	c.Env = agent.ScrubbedBuildEnv(os.Environ())
	configureMeasureProcGroup(c) // per-platform: own group + kill-group on cancel
	c.WaitDelay = measureWaitGrace

	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	// Read at most maxOut+1 bytes: the extra byte lets us DETECT an over-cap command
	// without buffering it unbounded.
	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxOut)+1))
	overCap := len(data) > maxOut
	if overCap {
		// Stop the chatty command immediately rather than draining it.
		killMeasureGroup(c)
	}
	waitErr := c.Wait()
	switch {
	case overCap:
		return nil, fmt.Errorf("measure output exceeded %d bytes; treated as failure", maxOut)
	case readErr != nil:
		return nil, fmt.Errorf("read measure output: %w", readErr)
	case waitErr != nil:
		return nil, waitErr
	}
	return data, nil
}

// parseMeasureMetrics extracts the first balanced JSON object from the command's
// stdout (reusing the injection-aware extractor the seat parser uses) and coerces
// its numeric entries into typed metrics. Non-numeric values are dropped rather
// than failing the whole parse; an object with no numeric metric is an error.
func parseMeasureMetrics(out []byte) (RoundMetrics, error) {
	obj, ok := agent.ExtractInnerJSON(string(out))
	if !ok {
		return RoundMetrics{}, errors.New("measure output contained no JSON object")
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(obj), &entries); err != nil {
		return RoundMetrics{}, fmt.Errorf("measure output is not a JSON object: %w", err)
	}
	values := map[string]float64{}
	for k, raw := range entries {
		var f float64
		if json.Unmarshal(raw, &f) == nil {
			values[k] = f
		}
	}
	if len(values) == 0 {
		return RoundMetrics{}, errors.New("measure output had no numeric metric")
	}
	return metricsFrom(values), nil
}

// mergeMetrics folds external metrics onto the round's base metrics; external keys
// win (the whole point of an external measure is to be authoritative). It never
// mutates either input.
func mergeMetrics(base, external map[string]float64) map[string]float64 {
	if len(external) == 0 {
		return base
	}
	out := make(map[string]float64, len(base)+len(external))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range external {
		out[k] = v
	}
	return out
}
