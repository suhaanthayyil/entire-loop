package org

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
)

// TestMeasureEdge_ParsesStubbedJSON: a stubbed exec returns a JSON object; the
// edge parses its numeric entries into typed metrics and drops non-numeric ones.
func TestMeasureEdge_ParsesStubbedJSON(t *testing.T) {
	t.Parallel()
	e := MeasureEdge{
		Cmd: "pretend-test-runner",
		Exec: func(_ context.Context, cmd string) ([]byte, error) {
			if cmd != "pretend-test-runner" {
				t.Errorf("exec got cmd %q", cmd)
			}
			return []byte(`{"progress": 0.8, "risk": 0.1, "label": "not-a-number", "tests_passing": 42}`), nil
		},
	}
	m, err := e.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if m.Progress() != 0.8 {
		t.Errorf("progress = %v, want 0.8", m.Progress())
	}
	if m.Risk() != 0.1 {
		t.Errorf("risk = %v, want 0.1", m.Risk())
	}
	if v, ok := m.Get("tests_passing"); !ok || v != 42 {
		t.Errorf("tests_passing = %v,%v, want 42,true", v, ok)
	}
	if _, ok := m.Get("label"); ok {
		t.Errorf("a non-numeric metric must be dropped, not coerced; label present")
	}
}

// TestMeasureEdge_RealShellStubCmd exercises the DEFAULT (unstubbed) executor by
// running a real `echo` command through `sh -c` — no claude, no stub Exec — so the
// production exec path itself is covered.
func TestMeasureEdge_RealShellStubCmd(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	e := MeasureEdge{Cmd: `echo '{"progress": 0.5, "coverage": 87.5}'`}
	m, err := e.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure via real sh: %v", err)
	}
	if m.Progress() != 0.5 {
		t.Errorf("progress = %v, want 0.5", m.Progress())
	}
	if v, ok := m.Get("coverage"); !ok || v != 87.5 {
		t.Errorf("coverage = %v,%v, want 87.5,true", v, ok)
	}
}

// TestMeasureEdge_ExtractsObjectFromNoisyOutput: leading log noise before the JSON
// object is tolerated (the injection-aware extractor finds the object).
func TestMeasureEdge_ExtractsObjectFromNoisyOutput(t *testing.T) {
	t.Parallel()
	e := MeasureEdge{
		Cmd: "noisy",
		Exec: func(context.Context, string) ([]byte, error) {
			return []byte("running tests...\nok\n{\"progress\": 1}\n"), nil
		},
	}
	m, err := e.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if m.Progress() != 1 {
		t.Errorf("progress = %v, want 1", m.Progress())
	}
}

// TestMeasureEdge_ErrorPaths: an empty command, an exec failure, non-JSON output,
// and a JSON object with no numeric metric each surface an error (so the loop
// degrades to its claude-derived metrics).
func TestMeasureEdge_ErrorPaths(t *testing.T) {
	t.Parallel()

	if _, err := (MeasureEdge{}).Measure(context.Background()); err == nil {
		t.Errorf("an empty command must error")
	}

	failExec := MeasureEdge{Cmd: "boom", Exec: func(context.Context, string) ([]byte, error) {
		return nil, errors.New("exit status 1")
	}}
	if _, err := failExec.Measure(context.Background()); err == nil {
		t.Errorf("an exec failure must error")
	}

	notJSON := MeasureEdge{Cmd: "x", Exec: func(context.Context, string) ([]byte, error) {
		return []byte("no json here"), nil
	}}
	if _, err := notJSON.Measure(context.Background()); err == nil {
		t.Errorf("non-JSON output must error")
	}

	noNumeric := MeasureEdge{Cmd: "x", Exec: func(context.Context, string) ([]byte, error) {
		return []byte(`{"status": "green"}`), nil
	}}
	if _, err := noNumeric.Measure(context.Background()); err == nil {
		t.Errorf("a JSON object with no numeric metric must error")
	}
}

// TestMeasureEdge_TimeoutKillsForkingGrandchild is the finding-1 guard: a command
// that forks a grandchild which inherits (and holds open) stdout must NOT be able to
// outlive the timeout. The default executor runs it in its own process group and
// kills the whole group on timeout, so the round stays bounded — where a naive
// c.Output() would block until the 30s sleep exited.
func TestMeasureEdge_TimeoutKillsForkingGrandchild(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	e := MeasureEdge{Cmd: "sleep 30 & echo queued", Timeout: 500 * time.Millisecond}
	start := time.Now()
	_, err := e.Measure(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Errorf("a timed-out measure must error, not silently succeed")
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout did not reap the forking grandchild: measure took %s (want well under 30s)", elapsed)
	}
}

// TestMeasureEdge_OutputCapDegrades is the finding-5 guard: a command that spews
// unbounded output (cat /dev/zero) must be capped and treated as a failure (degrade
// to claude metrics) rather than buffered until OOM, and killed promptly.
func TestMeasureEdge_OutputCapDegrades(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("/dev/zero not available")
	}
	e := MeasureEdge{Cmd: "cat /dev/zero", MaxOutputBytes: 1024, Timeout: 5 * time.Second}
	start := time.Now()
	_, err := e.Measure(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Errorf("an over-cap measure must error so the round degrades to claude metrics")
	}
	if elapsed > 10*time.Second {
		t.Errorf("over-cap command was not killed promptly: took %s", elapsed)
	}
}

// TestConvergeMetricMet is the finding-3 unit guard: metric-threshold convergence
// fires only when configured, only for the named metric, and honors the direction.
func TestConvergeMetricMet(t *testing.T) {
	t.Parallel()
	above := Options{ConvergeMetric: "progress", ConvergeAt: 0.8}
	if !above.convergeMetricMet(map[string]float64{"progress": 0.9}) {
		t.Errorf(">= threshold should be met at 0.9")
	}
	if above.convergeMetricMet(map[string]float64{"progress": 0.5}) {
		t.Errorf("0.5 is below 0.8; must not be met")
	}
	if above.convergeMetricMet(map[string]float64{"other": 1}) {
		t.Errorf("an absent metric must not be met")
	}
	if (Options{}).convergeMetricMet(map[string]float64{"progress": 1}) {
		t.Errorf("an unset ConvergeMetric must never converge")
	}
	below := Options{ConvergeMetric: "risk", ConvergeAt: 0.2, ConvergeBelow: true}
	if !below.convergeMetricMet(map[string]float64{"risk": 0.1}) {
		t.Errorf("<= threshold should be met at 0.1")
	}
	if below.convergeMetricMet(map[string]float64{"risk": 0.5}) {
		t.Errorf("0.5 is above 0.2; must not be met for the below direction")
	}
}

// TestLoop_ConvergeMetricStopsLoop is the finding-3 integration guard: with a
// metric threshold configured, an external metric crossing it meets the goal and
// STOPS the loop — even though the critic never asserts goal-met and fixed-count
// mode would otherwise run every round. Uses a stub Exec; never spawns claude.
func TestLoop_ConvergeMetricStopsLoop(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleCritic: {OK: true, GoalMet: false, Verdict: "not yet"},
		},
	}
	measure := &MeasureEdge{Cmd: "external", Exec: func(context.Context, string) ([]byte, error) {
		return []byte(`{"progress": 0.95}`), nil
	}}
	st, err := Run(context.Background(), Options{
		Goal: "stop on the external metric", Rounds: 5,
		Runner: fake, Briefer: staticBriefer{}, Store: newTestStore(t),
		Measure: measure, ConvergeMetric: "progress", ConvergeAt: 0.8,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !st.GoalMet {
		t.Errorf("crossing the metric threshold must meet the goal")
	}
	if st.Round != 1 {
		t.Errorf("metric-threshold convergence should stop after round 1; got round %d", st.Round)
	}
}

// TestMergeMetrics_ExternalWins: external metrics override the base per key and the
// union is returned; inputs are not mutated.
func TestMergeMetrics_ExternalWins(t *testing.T) {
	t.Parallel()
	base := map[string]float64{"progress": 0.2, "claude_only": 9}
	ext := map[string]float64{"progress": 0.9, "coverage": 80}
	got := mergeMetrics(base, ext)
	if got["progress"] != 0.9 {
		t.Errorf("external progress must win; got %v", got["progress"])
	}
	if got["claude_only"] != 9 || got["coverage"] != 80 {
		t.Errorf("merge should be the union; got %v", got)
	}
	if base["progress"] != 0.2 {
		t.Errorf("merge must not mutate the base map")
	}
	// No external metrics returns the base unchanged.
	if same := mergeMetrics(base, nil); same["progress"] != 0.2 {
		t.Errorf("nil external should leave base intact")
	}
}

// TestLoop_MeasureEdgeOverridesRoundMetrics is the integration guard: with a
// MeasureEdge configured, the round's persisted metrics carry the EXTERNAL value,
// overriding what the claude measure seat reported — the loop converges on the
// real signal. Uses a stub Exec; never spawns claude.
func TestLoop_MeasureEdgeOverridesRoundMetrics(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			// The claude measure seat claims progress 0.01 ...
			agent.RoleMeasure: {OK: true, Metrics: map[string]float64{"progress": 0.01}},
			agent.RoleCritic:  {OK: true, GoalMet: false},
		},
	}
	measure := &MeasureEdge{
		Cmd:  "external",
		Exec: func(context.Context, string) ([]byte, error) { return []byte(`{"progress": 0.99}`), nil },
	}
	st, err := Run(context.Background(), Options{
		Goal: "converge on a real metric", Rounds: 1,
		Runner: fake, Briefer: staticBriefer{}, Store: newTestStore(t), Measure: measure,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// ... but the persisted round + run metric is the EXTERNAL 0.99.
	if st.Metrics["progress"] != 0.99 {
		t.Errorf("external measure metric must override the claude one; run progress = %v, want 0.99", st.Metrics["progress"])
	}
	if st.Rounds[0].Metrics["progress"] != 0.99 {
		t.Errorf("round metric = %v, want the external 0.99", st.Rounds[0].Metrics["progress"])
	}
}

// TestLoop_MeasureEdgeFailureDegrades: when the external command fails, the round
// keeps its claude-derived metrics and the run does not error.
func TestLoop_MeasureEdgeFailureDegrades(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleMeasure: {OK: true, Metrics: map[string]float64{"progress": 0.3}},
			agent.RoleCritic:  {OK: true, GoalMet: false},
		},
	}
	measure := &MeasureEdge{Cmd: "boom", Exec: func(context.Context, string) ([]byte, error) {
		return nil, errors.New("nonzero exit")
	}}
	st, err := Run(context.Background(), Options{
		Goal: "measure failure degrades", Rounds: 1,
		Runner: fake, Briefer: staticBriefer{}, Store: newTestStore(t), Measure: measure,
	})
	if err != nil {
		t.Fatalf("a failed measure edge must not fail the run: %v", err)
	}
	if st.Rounds[0].Metrics["progress"] != 0.3 {
		t.Errorf("on measure failure the claude metric must stand; got %v", st.Rounds[0].Metrics["progress"])
	}
}
