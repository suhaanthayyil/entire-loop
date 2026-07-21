// Package briefing assembles the bounded per-seat brief handed to a worker on
// stdin. It shells the sibling plugins for structural + knowledge context
// (`entire graph symbols` and `entire brain query`), folds in the goal, the
// compacted run state, and prior metrics, and substitutes them into the seat's
// role template. Everything is bounded and degrades gracefully: if a sibling is
// down, slow, or hangs, the brief carries a note instead of failing.
//
// The package is named `briefing` (not `context`) to avoid shadowing the
// standard library `context` package it depends on.
package briefing

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
	"github.com/suhaanthayyil/entire-loop/internal/templates"
)

const (
	// DefaultMaxBytes hard-caps the assembled brief.
	DefaultMaxBytes = 64 * 1024
	// DefaultGraphHeadLines bounds how many ndjson symbol lines are taken from the
	// graph so a large repo does not blow the budget.
	DefaultGraphHeadLines = 200
	// siblingTimeout bounds each sibling-plugin shell-out so one hung sibling
	// cannot stall the whole loop: a brief is built per seat and a hang here would
	// mean loop.go's wg.Wait never returns. It mirrors the doctor probeTimeout. A
	// timeout surfaces as an exec error and falls through to the graceful note.
	siblingTimeout = 25 * time.Second
	// maxExecReadBytes hard-caps how much stdout defaultExec buffers from a
	// sibling plugin. The gather helpers further truncate to headLines/sectionCap;
	// this is the memory ceiling on the read itself, so a huge `graph symbols`
	// stream on a large repo cannot OOM the orchestrator. On overflow the child is
	// killed rather than drained.
	maxExecReadBytes = 1 << 20 // 1 MiB
)

// Env carries the resolved environment the brief builder needs. It is a small,
// dependency-free struct so briefing never imports the cli package.
type Env struct {
	RepoRoot string
	DataDir  string
}

// ExecFunc runs an external command and returns its stdout. It is injectable so
// tests can stub the sibling plugins.
type ExecFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// Builder implements org.Briefer. It captures the environment and (optionally) a
// stubbed exec function.
type Builder struct {
	Env            Env
	Exec           ExecFunc
	MaxBytes       int
	GraphHeadLines int
}

// Brief builds the brief for one seat. It satisfies org.Briefer. upstream carries
// the validated outputs of this seat's upstream nodes in the round DAG so data
// flows across the edge (e.g. build sees research's findings; critic sees build's
// proposal).
func (b Builder) Brief(ctx context.Context, goal string, st *state.State, seat agent.SeatSpec, upstream []state.SeatOutcome) (string, error) {
	return BuildBrief(ctx, b, goal, st, seat, upstream)
}

// BuildBrief assembles and renders the brief for a seat. It never hard-fails on a
// missing sibling; the returned error is non-nil only when the seat template
// itself cannot be rendered.
func BuildBrief(ctx context.Context, b Builder, goal string, st *state.State, seat agent.SeatSpec, upstream []state.SeatOutcome) (string, error) {
	execFn := b.Exec
	if execFn == nil {
		execFn = defaultExec
	}
	maxBytes := b.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	headLines := b.GraphHeadLines
	if headLines <= 0 {
		headLines = DefaultGraphHeadLines
	}

	sectionCap := maxBytes / 2

	// Bind the graph invocation to the SEAT's repo root when it carries one. This
	// is what keeps a MUTATING build seat's brief pointed at the throwaway CLONE
	// (the loop sets seat.RepoRoot = clone dir before briefing) instead of leaking
	// the real repo path to the bypass worker; every other seat carries the real
	// root and is unaffected.
	graphRoot := seat.RepoRoot
	if graphRoot == "" {
		graphRoot = b.Env.RepoRoot
	}
	graph := gatherGraph(ctx, execFn, graphRoot, headLines, sectionCap)
	brainCtx := gatherBrainQuery(ctx, execFn, goal, sectionCap)

	values := map[string]string{
		templates.MarkerGoal:        strings.TrimSpace(goal),
		templates.MarkerState:       compactState(st),
		templates.MarkerGraph:       graph,
		templates.MarkerBrief:       brainCtx,
		templates.MarkerMetrics:     metricsBlock(st),
		templates.MarkerUpstream:    upstreamBlock(upstream, sectionCap),
		templates.MarkerLens:        strings.TrimSpace(seat.Lens),
		templates.MarkerRefinedGoal: refinedGoalBlock(st),
		templates.MarkerSubgoals:    subgoalsBlock(st),
		templates.MarkerFocus:       focusBlock(seat.Focus),
	}

	rendered, err := templates.Render(seat.Role, values)
	if err != nil {
		return "", err
	}
	return boundString(rendered, maxBytes), nil
}

// upstreamBlock renders the validated output of a seat's upstream nodes for the
// ${UPSTREAM} marker: each upstream seat's findings AND its full proposal (the
// actual diff, not just a byte count — a downstream critic must see the real
// change to verify it). Bounded to the section budget.
func upstreamBlock(upstream []state.SeatOutcome, limit int) string {
	if len(upstream) == 0 {
		return "(no upstream seat output this round)"
	}
	var b strings.Builder
	for _, seat := range upstream {
		fmt.Fprintf(&b, "== %s ==\n", seat.Role)
		for _, f := range seat.Findings {
			fmt.Fprintf(&b, "- %s\n", oneLine(truncate(f, 400)))
		}
		if strings.TrimSpace(seat.Proposal) != "" {
			fmt.Fprintf(&b, "proposal:\n%s\n", truncate(seat.Proposal, limit/2))
		}
		if seat.Verdict != "" {
			fmt.Fprintf(&b, "verdict: %s\n", oneLine(truncate(seat.Verdict, 400)))
		}
	}
	return boundString(strings.TrimSpace(b.String()), limit)
}

// gatherGraph shells `entire graph symbols` under its own per-call timeout and
// returns a bounded head of its ndjson output. defaultExec caps the bytes read
// so a huge stream cannot OOM the orchestrator; here we further truncate to
// headLines and the section byte budget. On error or timeout it returns a
// graceful note rather than failing.
func gatherGraph(ctx context.Context, execFn ExecFunc, repoRoot string, headLines, limit int) string {
	repo := repoRoot
	if repo == "" {
		repo = "."
	}
	tctx, cancel := context.WithTimeout(ctx, siblingTimeout)
	defer cancel()
	out, err := execFn(tctx, "entire", "graph", "symbols", "--repo", repo, "--format", "ndjson")
	if err != nil {
		return fmt.Sprintf("(graph unavailable: %s)", oneLine(err.Error()))
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > headLines {
		truncated := len(lines) - headLines
		lines = lines[:headLines]
		lines = append(lines, fmt.Sprintf("... (%d symbol lines truncated)", truncated))
	}
	return boundString(strings.Join(lines, "\n"), limit)
}

// gatherBrainQuery shells `entire brain query <goal>` (hybrid RRF search — the
// verified-working brain subcommand; `brain context` is not a real subcommand)
// under its own per-call timeout. On error OR timeout (a hang is bounded by the
// timeout ctx and surfaces as an error) it returns a graceful note rather than
// failing, so a slow or missing brain never stalls the loop.
func gatherBrainQuery(ctx context.Context, execFn ExecFunc, goal string, limit int) string {
	tctx, cancel := context.WithTimeout(ctx, siblingTimeout)
	defer cancel()
	out, err := execFn(tctx, "entire", "brain", "query", goal)
	if err != nil {
		return fmt.Sprintf("(brain context unavailable: %s)", oneLine(err.Error()))
	}
	return boundString(strings.TrimSpace(string(out)), limit)
}

// compactState renders a small, deterministic summary of prior rounds for the
// ${STATE} marker: the goal-met flag, the latest verdict, and each prior seat's
// most recent findings, all bounded.
func compactState(st *state.State) string {
	if st == nil || len(st.Rounds) == 0 {
		return "(no prior rounds)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "rounds so far: %d; goal_met: %v\n", st.Round, st.GoalMet)
	last := st.Rounds[len(st.Rounds)-1]
	if last.Verdict != "" {
		fmt.Fprintf(&b, "latest verdict: %s\n", oneLine(truncate(last.Verdict, 400)))
	}
	for _, seat := range last.Seats {
		if len(seat.Findings) == 0 && seat.Proposal == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s:", seat.Role)
		for _, f := range seat.Findings {
			fmt.Fprintf(&b, " %s;", oneLine(truncate(f, 200)))
		}
		if seat.Proposal != "" {
			fmt.Fprintf(&b, " [proposed diff: %d bytes]", len(seat.Proposal))
		}
		b.WriteString("\n")
	}
	return truncate(b.String(), 8*1024)
}

// metricsBlock renders the run-level metrics for the ${METRICS} marker.
func metricsBlock(st *state.State) string {
	if st == nil || len(st.Metrics) == 0 {
		return "(no metrics yet)"
	}
	keys := make([]string, 0, len(st.Metrics))
	for k := range st.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%.4g", k, st.Metrics[k]))
	}
	return strings.Join(parts, ", ")
}

// refinedGoalBlock renders the control plane's refined goal from state for the
// ${REFINED_GOAL} marker. It is empty (collapses to nothing) under the fixed
// planner or before the first refinement, so a worker template that carries the
// marker shows the block only once a control seat has refined the goal.
func refinedGoalBlock(st *state.State) string {
	if st == nil {
		return ""
	}
	rg := strings.TrimSpace(st.RefinedGoal)
	if rg == "" {
		return ""
	}
	return "Refined goal (control plane):\n" + truncate(rg, 4*1024)
}

// subgoalsBlock renders the control plane's subgoals from state for the
// ${SUBGOALS} marker, one per line. Empty when there are none.
func subgoalsBlock(st *state.State) string {
	if st == nil || len(st.Subgoals) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Subgoals (control plane):")
	for _, s := range st.Subgoals {
		s = oneLine(truncate(s, 400))
		if s == "" {
			continue
		}
		fmt.Fprintf(&b, "\n- %s", s)
	}
	if b.Len() == len("Subgoals (control plane):") {
		return ""
	}
	return b.String()
}

// focusBlock renders a seat's per-round focus text for the ${FOCUS} marker. The
// focus is opaque prompt text (bounded by the planner) that only ever lands in the
// brief body — never as a flag or command — so it needs no escaping here, only a
// label. Empty focus collapses to nothing.
func focusBlock(focus string) string {
	f := strings.TrimSpace(focus)
	if f == "" {
		return ""
	}
	return "Your focus this round (from the control plane):\n" + f
}

// defaultExec streams a sibling command's stdout through a byte-bounded reader
// instead of buffering the whole thing: it reads at most maxExecReadBytes and, if
// the stream is larger, stops and kills the child rather than draining (or
// blocking on) an unbounded stream. This is what keeps a huge `graph symbols`
// ndjson dump from OOMing the orchestrator.
func defaultExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Read one byte past the cap so we can detect (and stop) an overflowing stream.
	data, readErr := io.ReadAll(io.LimitReader(stdout, maxExecReadBytes+1))
	overflow := len(data) > maxExecReadBytes
	if overflow {
		data = data[:maxExecReadBytes]
		cancel() // we have enough; kill the child so Wait cannot stall on a full pipe
	}
	waitErr := cmd.Wait()

	switch {
	case overflow:
		return data, nil // killed on purpose after reading enough — not a real failure
	case readErr != nil:
		return data, readErr
	default:
		return data, waitErr
	}
}

func boundString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return safeCut(s, max)
	}
	return safeCut(s, max-3) + "..."
}

// safeCut returns s truncated to at most n bytes, backed up to the nearest rune
// boundary at or before n so the result is always valid UTF-8 (never a byte
// slice through the middle of a multi-byte rune).
func safeCut(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func truncate(s string, max int) string { return boundString(s, max) }

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}
