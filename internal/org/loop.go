package org

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// Briefer assembles the per-seat brief handed to a worker on stdin. upstream
// carries the validated outputs of this seat's upstream nodes in the round DAG so
// data flows across the edge (build sees research; critic sees build's proposal).
// The loop depends on this interface so tests can supply a recording brief and
// never shell out to `entire graph`/`entire brain`.
type Briefer interface {
	Brief(ctx context.Context, goal string, st *state.State, seat agent.SeatSpec, upstream []state.SeatOutcome) (string, error)
}

const (
	// DefaultMaxRounds is the safety cap on the converge loop.
	DefaultMaxRounds = 6
	// DefaultDryStreak is how many consecutive rounds with no new progress end the
	// run in converge mode.
	DefaultDryStreak = 2
)

// Options configures a loop run. Runner, Briefer, and Store are required; the
// rest have sensible defaults.
type Options struct {
	Goal string
	// Rounds, when > 0, selects FIXED-COUNT mode: run exactly this many rounds
	// (stopping early only on goal-met). When 0, the loop runs in CONVERGE mode
	// until goal-met, a dry streak, or MaxRounds.
	Rounds int
	// MaxRounds is the converge-mode safety cap (default DefaultMaxRounds).
	MaxRounds int
	// DryStreakLimit is how many consecutive dry rounds end a converge run
	// (default DefaultDryStreak).
	DryStreakLimit int
	Jobs           int
	Model          string
	Effort         string
	RepoRoot       string

	// PlanMode is the plan-mutability axis (orthogonal to the --planner choice):
	// PlanModeDynamic (default) re-plans the DAG every round; PlanModeImmutable
	// freezes the DAG after the first plan, disables runtime reorg, and executes it
	// with bounded per-node recovery.
	PlanMode PlanMode
	// NodeRetries is the bounded per-node recovery budget. It is applied only when
	// > 0 (set automatically in immutable plan-mode); dynamic mode leaves it 0, so a
	// failed node is re-planned rather than retried and every existing run is byte
	// -for-byte unchanged.
	NodeRetries int
	// Measure, when non-nil and enabled, is the external-metric MeasureEdge run
	// after each round; its typed metrics override the round's claude-derived ones.
	Measure *MeasureEdge

	// ConvergeMetric, when non-empty, enables metric-threshold convergence: a round's
	// goal is met once the metric named here crosses ConvergeAt (>= by default, or
	// <= when ConvergeBelow is set). This is the ONLY way an external measurement can
	// STOP the loop; leaving it empty keeps convergence on verdict/dry-streak alone.
	// It reads the round's FINAL metrics (after the MeasureEdge override), so it
	// reacts to the real external signal when one is configured.
	ConvergeMetric string
	ConvergeAt     float64
	ConvergeBelow  bool

	Runner  agent.Runner
	Briefer Briefer
	Planner Planner
	Reorg   Reorg
	Store   *state.Store

	Now    func() time.Time
	Stdout io.Writer
}

// Run drives the loop until it converges. Each round is planned, reorganized, and
// executed as a DATA-CARRYING pipeline (research → build → critic →
// measure/synthesize) with a router and a verifier-on-edge. The run stops when the
// goal is met, when it goes dry (converge mode), or at the round cap.
func Run(ctx context.Context, opts Options) (*state.State, error) {
	if opts.Runner == nil {
		return nil, errors.New("loop: a Runner is required")
	}
	if opts.Briefer == nil {
		return nil, errors.New("loop: a Briefer is required")
	}
	if opts.Store == nil {
		return nil, errors.New("loop: a Store is required")
	}
	if opts.Goal == "" {
		return nil, errors.New("loop: a goal is required")
	}
	if opts.Jobs <= 0 {
		opts.Jobs = 2
	}
	if opts.Planner == nil {
		opts.Planner = FixedPlanner{Model: opts.Model, Effort: opts.Effort, RepoRoot: opts.RepoRoot}
	}
	if opts.Reorg == nil {
		opts.Reorg = NoopReorg{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}

	// Immutable plan-mode: the seat/node DAG is decided once up front and frozen —
	// runtime reorg (a dynamic re-plan mechanism) is disabled, and per-node recovery
	// (retry then degrade) replaces re-planning as the failure response.
	if opts.PlanMode == PlanModeImmutable {
		opts.Reorg = NoopReorg{}
		if opts.NodeRetries <= 0 {
			opts.NodeRetries = defaultNodeRetries
		}
	}

	fixedCount := opts.Rounds > 0
	maxRounds := opts.MaxRounds
	if fixedCount {
		maxRounds = opts.Rounds
	}
	if maxRounds <= 0 {
		maxRounds = DefaultMaxRounds
	}
	dryLimit := opts.DryStreakLimit
	if dryLimit <= 0 {
		dryLimit = DefaultDryStreak
	}

	st, err := opts.Store.Load()
	if err != nil {
		return nil, err
	}
	if st == nil {
		st = opts.Store.NewState(opts.Goal, opts.Now())
	}
	if err := opts.Store.Save(st); err != nil {
		return nil, err
	}

	// The CycleEdge owns the loop-until-dry convergence state machine.
	cycle := newCycleEdge(st, dryLimit, fixedCount)
	// frozen holds the immutable-mode DAG once captured. On a RESUME (new process)
	// it is rehydrated from the persisted roster so immutable truly plans the DAG
	// ONCE across process boundaries rather than re-planning a different one.
	frozen := &frozenRoster{}
	if opts.PlanMode == PlanModeImmutable && len(st.FrozenSeats) > 0 {
		frozen.seats = seatsFromFrozen(st.FrozenSeats)
		frozen.captured = true
	}
	stopped := false
	start := st.Round + 1
	for round := start; round <= maxRounds && !stopped; round++ {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		seats := opts.planSeats(ctx, round, st, frozen)
		roundState, rawKeys := opts.runRound(ctx, round, st, seats)
		state.Merge(st, roundState)
		if err := opts.Store.Save(st); err != nil {
			return st, err
		}
		printRoundSummary(opts.Stdout, roundState)

		// A round entirely refused under no-egress is futile; surface it as a
		// non-zero exit so scripts detect the refusal rather than a silent success.
		if roundRefusedForNoEgress(roundState) {
			return st, fmt.Errorf("loop: round %d refused: %s", round, firstNoEgressWarning(roundState))
		}

		dec := cycle.decide(round, roundState, rawKeys)
		if dec.message != "" {
			fmt.Fprint(opts.Stdout, dec.message)
		}
		switch dec.action {
		case cycleStopGoalMet:
			return st, nil
		case cycleStop:
			stopped = true
		}
	}
	if !st.GoalMet {
		fmt.Fprintf(opts.Stdout, "loop: goal not met after %d round(s)\n", st.Round)
	}
	return st, nil
}

// planSeats resolves the round's seat roster. In dynamic plan-mode it re-plans
// every round (planner + runtime reorg). In immutable plan-mode it captures the
// first round's roster (reorg already disabled by Run) and returns that same DAG,
// re-stamped with the round, every round after — no mid-run re-plan. The captured
// roster is also persisted into state so a resume rehydrates the SAME DAG.
//
// Capture is tracked with an explicit `captured` bool, NOT `frozen.seats != nil`: a
// legitimately empty first-round roster clones to a nil slice, which the nil check
// would misread as "not yet captured" and re-plan every round.
func (opts Options) planSeats(ctx context.Context, round int, st *state.State, frozen *frozenRoster) []agent.SeatSpec {
	if opts.PlanMode == PlanModeImmutable && frozen.captured {
		return stampRound(cloneSeats(frozen.seats), round)
	}
	seats := opts.Planner.Plan(ctx, opts.Goal, st)
	seats = opts.Reorg.Apply(seats, st)
	if opts.PlanMode == PlanModeImmutable {
		frozen.seats = cloneSeats(seats)
		frozen.captured = true
		// Persist the roster; the round's Store.Save (in Run) writes it to disk so a
		// resumed process rehydrates this exact DAG instead of re-planning.
		st.FrozenSeats = frozenFromSeats(frozen.seats)
	}
	return stampRound(seats, round)
}

// runRound executes a single pre-planned round as a pipeline of typed EDGES and
// returns its RoundState plus the convergence keys it surfaced. The seat roster is
// resolved by the caller (planSeats) so plan-mutability is decided once, out of
// band. Edges wire the round: DataEdge carries each seat's upstream, ConditionalEdge
// routes the verify path, VerifierEdge gates a large proposal, MeasureEdge folds in
// an external metric.
func (opts Options) runRound(ctx context.Context, round int, st *state.State, seats []agent.SeatSpec) (state.RoundState, []string) {
	started := opts.Now().UTC()
	byRole := indexByRole(seats)

	// art records any mutating-build clone produced this round so the external
	// MeasureEdge can measure the clone (which HAS the round's proposed change)
	// before it is torn down. Its clones are cleaned up at end of round, AFTER the
	// measure step — the loop stays propose-only (nothing is applied to repoRoot).
	art := &buildArtifact{}
	defer art.cleanupAll()

	var results []agent.SeatResult

	// research → build → critic, strictly sequenced so each consumes its upstream
	// via a DataEdge (output → input).
	if s, ok := byRole[agent.RoleResearch]; ok {
		results = append(results, opts.runSeatRecovered(ctx, s, st, nil, art))
	}
	if s, ok := byRole[agent.RoleBuild]; ok {
		up := DataEdge{From: []string{agent.RoleResearch}}.Project(results)
		results = append(results, opts.runSeatRecovered(ctx, s, st, up, art))
	}

	// ConditionalEdge (router): inspect the validated build proposal and route the
	// verification.
	rt := ConditionalEdge{}.Route(RoundView{Round: round, Results: results, State: st})

	if s, ok := byRole[agent.RoleCritic]; ok {
		up := DataEdge{From: []string{agent.RoleResearch, agent.RoleBuild}}.Project(results)
		results = append(results, opts.runSeatRecovered(ctx, s, st, up, art))
	}

	// VerifierEdge (adversarial gate): for the full-audit route with a concrete
	// proposal, audit it before it may be accepted into the result.
	buildRes, hasBuild := lastByRole(results, agent.RoleBuild)
	audit := verifierResult{accepted: true}
	if rt == routeFullAudit && hasBuild && strings.TrimSpace(buildRes.Proposal) != "" {
		up := DataEdge{From: []string{agent.RoleResearch, agent.RoleBuild}}.Project(results)
		audit = VerifierEdge{}.Gate(ctx, opts, round, st, up)
		results = append(results, audit.seats...)
	}

	if s, ok := byRole[agent.RoleMeasure]; ok {
		up := DataEdge{From: []string{agent.RoleResearch, agent.RoleBuild, agent.RoleCritic}}.Project(results)
		results = append(results, opts.runSeatRecovered(ctx, s, st, up, art))
	}
	if s, ok := byRole[agent.RoleSynthesize]; ok {
		results = append(results, opts.runSeatRecovered(ctx, s, st, allOutcomes(results), art))
	}

	// Convergence keys: everything surfaced this round, accepted or not — so a
	// rejected item that reappears next round reads as "already seen".
	rawKeys := resultKeys(results)

	rs := state.RoundState{Round: round, StartedAt: started, Route: rt.String()}
	for _, res := range results {
		oc := toOutcome(res)
		// If the verifier refuted the proposal, drop it from the committed result —
		// but persist its stable dedupe key first, so a process-resume rebuilds the
		// convergence `seen` set correctly and the dropped proposal never resurfaces.
		if res.Role == agent.RoleBuild && audit.ran && !audit.accepted {
			oc.ProposalKey = proposalKey(oc.Proposal)
			oc.Proposal = ""
			oc.Warnings = append(oc.Warnings, fmt.Sprintf(
				"proposal refuted by verifier majority (%d/%d survived); dropped from result", audit.survivors, audit.total))
		}
		rs.Seats = append(rs.Seats, oc)
		rs.CostUSD += res.CostUSD
	}
	rs.Metrics = metricsFromSeats(results)
	// MeasureEdge (external signal): when configured, run the user's command and let
	// its typed metrics OVERRIDE the round's claude-derived ones, so the reorg edge
	// (and, when a threshold is configured, convergence) react to a real external
	// measurement. It measures the mutating-build CLONE when one was produced this
	// round (art.measureDir) — the loop is propose-only, so measuring repoRoot would
	// yield the SAME numbers every round; the clone is the only place the round's
	// actual change exists. A non-mutating round measures repoRoot as a static
	// baseline. It is SKIPPED under no-egress / on a refused round (a refused round
	// must not shell out) and degrades gracefully on failure — the round keeps its
	// claude metrics and the failure is logged.
	egressBlocked := agent.NoEgressMode() || roundRefusedForNoEgress(rs)
	if opts.Measure != nil && opts.Measure.Enabled() && !egressBlocked {
		me := *opts.Measure
		if art.measureDir != "" {
			me.Dir = art.measureDir
		}
		if m, err := me.Measure(ctx); err != nil {
			fmt.Fprintf(opts.Stdout, "measure-edge: external command failed: %v\n", err)
		} else {
			rs.Metrics = mergeMetrics(rs.Metrics, m.ToMap())
		}
	}
	verdictText, goalMet := verdictFromSeats(results)
	rs.Verdict = verdictText
	// Goal-met requires the verifier to have accepted the work (when it ran) — OR the
	// configured external metric crossing its threshold (metric-threshold
	// convergence), which is authoritative when set.
	rs.GoalMet = goalMet && audit.accepted
	if opts.convergeMetricMet(rs.Metrics) {
		rs.GoalMet = true
	}
	rs.EndedAt = opts.Now().UTC()
	return rs, rawKeys
}

// convergeMetricMet reports whether the round's final metrics cross the configured
// convergence threshold. It returns false when metric-threshold convergence is not
// configured (empty ConvergeMetric) or the metric was not measured this round.
func (opts Options) convergeMetricMet(metrics map[string]float64) bool {
	if opts.ConvergeMetric == "" {
		return false
	}
	v, ok := metrics[opts.ConvergeMetric]
	if !ok {
		return false
	}
	if opts.ConvergeBelow {
		return v <= opts.ConvergeAt
	}
	return v >= opts.ConvergeAt
}

// runSeatRecovered runs a seat with the bounded per-node recovery of immutable
// plan-mode: if the seat degrades (not-OK) it is retried up to Options.NodeRetries
// times (honoring cancellation), each retry tagged with a warning; a node that is
// still not-OK after the budget is left degraded. With NodeRetries == 0 (dynamic
// plan-mode) it is a pass-through to runSeat, so dynamic runs are unchanged.
func (opts Options) runSeatRecovered(ctx context.Context, seat agent.SeatSpec, st *state.State, upstream []state.SeatOutcome, art *buildArtifact) agent.SeatResult {
	res := opts.runSeat(ctx, seat, st, upstream, art)
	for attempt := 1; !res.OK && attempt <= opts.NodeRetries; attempt++ {
		if ctx.Err() != nil {
			break
		}
		// Carry the failed attempt's warnings forward so retry diagnostics are not
		// lost when the next attempt replaces the whole SeatResult.
		prior := res.Warnings
		res = opts.runSeat(ctx, seat, st, upstream, art)
		merged := make([]string, 0, len(prior)+len(res.Warnings)+1)
		merged = append(merged, prior...)
		merged = append(merged, res.Warnings...)
		merged = append(merged, fmt.Sprintf(
			"immutable plan-mode: node %q retried (attempt %d of %d) after failure", seat.Role, attempt, opts.NodeRetries))
		res.Warnings = merged
	}
	return res
}

// runSeat builds the seat's brief and runs it, converting any panic in either the
// Briefer or the Runner into a not-OK SeatResult so one bad seat never crashes the
// process or wedges the round.
func (opts Options) runSeat(ctx context.Context, seat agent.SeatSpec, st *state.State, upstream []state.SeatOutcome, art *buildArtifact) (res agent.SeatResult) {
	defer func() {
		if rec := recover(); rec != nil {
			res = agent.SeatResult{
				Role:     seat.Role,
				OK:       false,
				Warnings: []string{fmt.Sprintf("seat panicked: %v", rec)},
			}
		}
	}()
	// A mutating (build) seat runs under REAL isolation: clone the target, bind the
	// brief to the clone, run the bypass worker there. Only a runner that supports
	// it (ExecRunner) takes this path; a test fake falls through to the plain path.
	if seat.Mutating {
		if mr, ok := opts.Runner.(agent.MutatingBuildRunner); ok {
			return opts.runMutatingSeat(ctx, mr, seat, st, upstream, art)
		}
	}
	brief, berr := opts.Briefer.Brief(ctx, opts.Goal, st, seat, upstream)
	res = opts.Runner.Run(ctx, seat, brief)
	if berr != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("brief degraded: %v", berr))
	}
	return res
}

// runMutatingSeat runs the build seat under REAL isolation. It clones the target
// into a confined OS-temp throwaway, binds the worker's brief to the CLONE (so the
// bypass worker never sees the real repo path in its brief, graph invocation, or
// cwd), runs the bypass worker there, and removes the clone on every path
// (normal, cancel, panic). If the target is not a git repo or the clone fails, it
// degrades to a non-mutating plan-mode seat against the real repo (read-only).
func (opts Options) runMutatingSeat(ctx context.Context, mr agent.MutatingBuildRunner, seat agent.SeatSpec, st *state.State, upstream []state.SeatOutcome, art *buildArtifact) agent.SeatResult {
	repoRoot := seat.RepoRoot
	if repoRoot == "" {
		repoRoot = opts.RepoRoot
	}
	if !agent.IsGitRepo(ctx, repoRoot) {
		return opts.runNonMutatingFallback(ctx, mr, seat, st, upstream,
			"mutating build unavailable: target is not a git repo; fell back to non-mutating propose-as-text")
	}
	clone, cleanup, err := agent.NewBuildClone(ctx, repoRoot)
	if err != nil {
		return opts.runNonMutatingFallback(ctx, mr, seat, st, upstream,
			fmt.Sprintf("mutating build degraded (%v); fell back to non-mutating propose-as-text", err))
	}
	// Teardown is deferred to END OF ROUND (art), not here: the external MeasureEdge
	// must be able to measure this clone — which HAS the round's change — before it
	// is removed. When no round-level artifact is threaded in (art == nil), fall back
	// to immediate teardown so the clone never leaks. Either way it is torn down
	// within the round; cleanup is context-free, so a cancelled round still reaps it.
	if art != nil {
		art.record(clone.Dir, cleanup)
	} else {
		defer cleanup()
	}

	// Bind the brief to the CLONE: the bypass worker's graph invocation, cwd, and
	// context reference the clone dir, never the real repo path.
	cloneSeat := seat
	cloneSeat.RepoRoot = clone.Dir
	brief, berr := opts.Briefer.Brief(ctx, opts.Goal, st, cloneSeat, upstream)
	res := mr.RunBypassInClone(ctx, cloneSeat, brief, clone)
	if berr != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("brief degraded: %v", berr))
	}
	return res
}

// runNonMutatingFallback runs the seat as a plain plan-mode (read-only) worker
// against the real repo and appends the given degrade warning. It is the safe
// landing when isolation is unavailable.
func (opts Options) runNonMutatingFallback(ctx context.Context, r agent.Runner, seat agent.SeatSpec, st *state.State, upstream []state.SeatOutcome, warn string) agent.SeatResult {
	plan := seat
	plan.Mutating = false
	brief, berr := opts.Briefer.Brief(ctx, opts.Goal, st, plan, upstream)
	res := r.Run(ctx, plan, brief)
	if berr != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("brief degraded: %v", berr))
	}
	res.Warnings = append(res.Warnings, warn)
	return res
}

// buildArtifact records the mutating-build clone(s) produced during a round so the
// external MeasureEdge can measure the clone that HAS the round's proposed change
// (the loop is propose-only — the change is never applied to repoRoot). Cleanups
// accumulate (immutable-mode retries can create several clones) and all run at end
// of round, AFTER the measure step; measureDir is the most recent clone.
type buildArtifact struct {
	measureDir string
	cleanups   []func()
}

// record registers a clone dir and its teardown. The latest clone wins as the
// measure dir; every clone is torn down by cleanupAll.
func (b *buildArtifact) record(dir string, cleanup func()) {
	if b == nil {
		return
	}
	b.measureDir = dir
	b.cleanups = append(b.cleanups, cleanup)
}

// cleanupAll tears down every clone recorded this round.
func (b *buildArtifact) cleanupAll() {
	if b == nil {
		return
	}
	for _, c := range b.cleanups {
		if c != nil {
			c()
		}
	}
}

// frozenRoster holds the immutable-plan-mode DAG once it has been captured. The
// `captured` bool — NOT a nil check on seats — is the sentinel: a legitimately
// empty roster must still read as captured so it is never re-planned every round.
type frozenRoster struct {
	seats    []agent.SeatSpec
	captured bool
}

// frozenFromSeats projects a live seat roster to the persisted, agent-free form so
// state carries no dependency on the agent package.
func frozenFromSeats(seats []agent.SeatSpec) []state.FrozenSeat {
	out := make([]state.FrozenSeat, 0, len(seats))
	for _, s := range seats {
		out = append(out, state.FrozenSeat{
			Role:      s.Role,
			BriefOnly: s.BriefOnly,
			McpBrain:  s.McpBrain,
			Mutating:  s.Mutating,
			Model:     s.Model,
			Effort:    s.Effort,
			RepoRoot:  s.RepoRoot,
			Lens:      s.Lens,
			Focus:     s.Focus,
		})
	}
	return out
}

// seatsFromFrozen rebuilds a live seat roster from the persisted form on resume.
// Round is left zero — the loop re-stamps it every round via stampRound.
func seatsFromFrozen(frozen []state.FrozenSeat) []agent.SeatSpec {
	out := make([]agent.SeatSpec, 0, len(frozen))
	for _, s := range frozen {
		out = append(out, agent.SeatSpec{
			Role:      s.Role,
			BriefOnly: s.BriefOnly,
			McpBrain:  s.McpBrain,
			Mutating:  s.Mutating,
			Model:     s.Model,
			Effort:    s.Effort,
			RepoRoot:  s.RepoRoot,
			Lens:      s.Lens,
			Focus:     s.Focus,
		})
	}
	return out
}

// fanOutSeats runs seats concurrently, bounded by Jobs, and returns their results
// in the seats' order. Each seat runs through runSeat, so a panic in any goroutine
// is recovered and wg.Wait always unblocks.
func (opts Options) fanOutSeats(ctx context.Context, seats []agent.SeatSpec, st *state.State, upstream []state.SeatOutcome) []agent.SeatResult {
	results := make([]agent.SeatResult, len(seats))
	sem := make(chan struct{}, opts.Jobs)
	var wg sync.WaitGroup
	for i, seat := range seats {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, seat agent.SeatSpec) {
			defer wg.Done()
			defer func() { <-sem }()
			// Verifier seats fanned out here are never mutating, so no build clone is
			// produced — nil artifact.
			results[i] = opts.runSeat(ctx, seat, st, upstream, nil)
		}(i, seat)
	}
	wg.Wait()
	return results
}

// lastByRole returns the most recent result for a role.
func lastByRole(results []agent.SeatResult, role string) (agent.SeatResult, bool) {
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].Role == role {
			return results[i], true
		}
	}
	return agent.SeatResult{}, false
}

// allOutcomes projects every result into an outcome, for the terminal synthesize
// seat's upstream.
func allOutcomes(results []agent.SeatResult) []state.SeatOutcome {
	out := make([]state.SeatOutcome, 0, len(results))
	for _, r := range results {
		out = append(out, toOutcome(r))
	}
	return out
}

// roundRefusedForNoEgress reports whether a round was entirely refused under
// no-egress: no seat succeeded and at least one carries the gate refusal.
func roundRefusedForNoEgress(rs state.RoundState) bool {
	if len(rs.Seats) == 0 {
		return false
	}
	refused := false
	for _, s := range rs.Seats {
		if s.OK {
			return false
		}
		if hasSubstr(s.Warnings, "no_egress") {
			refused = true
		}
	}
	return refused
}

func firstNoEgressWarning(rs state.RoundState) string {
	for _, s := range rs.Seats {
		for _, w := range s.Warnings {
			if strings.Contains(w, "no_egress") {
				return w
			}
		}
	}
	return "no-egress gate"
}

func hasSubstr(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// toOutcome projects an agent.SeatResult into the persisted state.SeatOutcome.
func toOutcome(r agent.SeatResult) state.SeatOutcome {
	return state.SeatOutcome{
		Role:     r.Role,
		OK:       r.OK,
		Warnings: r.Warnings,
		CostUSD:  r.CostUSD,
		NumTurns: r.NumTurns,
		Findings: r.Findings,
		Proposal: r.Proposal,
		Metrics:  r.Metrics,
		Verdict:  r.Verdict,
		GoalMet:  r.GoalMet,
	}
}

// metricsFromSeats returns the measure seat's metrics, or nil when absent.
func metricsFromSeats(results []agent.SeatResult) map[string]float64 {
	for _, r := range results {
		if r.Role == agent.RoleMeasure && len(r.Metrics) > 0 {
			return r.Metrics
		}
	}
	return nil
}

// verdictFromSeats derives the round verdict and goal-met flag. A synthesize seat
// wins when present; otherwise the critic decides. Goal-met requires that seat to
// be OK and to assert it.
func verdictFromSeats(results []agent.SeatResult) (string, bool) {
	for _, role := range []string{agent.RoleSynthesize, agent.RoleCritic} {
		for _, r := range results {
			if r.Role == role {
				return r.Verdict, r.OK && r.GoalMet
			}
		}
	}
	return "", false
}

func printRoundSummary(w io.Writer, rs state.RoundState) {
	fmt.Fprintf(w, "round %d: seats=%d route=%s goal_met=%v cost=$%.4f\n", rs.Round, len(rs.Seats), rs.Route, rs.GoalMet, rs.CostUSD)
	if rs.Verdict != "" {
		fmt.Fprintf(w, "  verdict: %s\n", rs.Verdict)
	}
	if len(rs.Metrics) > 0 {
		fmt.Fprintf(w, "  metrics: %s\n", formatMetrics(rs.Metrics))
	}
}

// formatMetrics renders a metrics map deterministically (sorted keys).
func formatMetrics(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%.3g", k, m[k]))
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}
