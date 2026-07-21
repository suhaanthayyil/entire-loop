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

	seen := seenFromState(st)
	dryStreak := 0
	failStreak := 0
	start := st.Round + 1
	for round := start; round <= maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		roundState, rawKeys := opts.runRound(ctx, round, st)
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

		if roundState.GoalMet {
			fmt.Fprintf(opts.Stdout, "loop: goal met after round %d — stopping\n", round)
			return st, nil
		}

		if fixedCount {
			continue
		}

		// A round where EVERY content (progress-role) seat failed or degraded is not
		// convergence — it is a failed round. It must not count toward the dry streak
		// (else a broken run reads as "converged — no new progress"). Track it as a
		// separate failed streak with a DISTINCT stop reason, and do not fold its
		// keys into `seen`.
		if !roundHadOKProgress(roundState.Seats) {
			dryStreak = 0
			failStreak++
			if failStreak >= dryLimit {
				fmt.Fprintf(opts.Stdout, "loop: stopping after %d consecutive failed round(s) — no content seat succeeded (round %d)\n", failStreak, round)
				break
			}
			continue
		}
		failStreak = 0

		newCount := seen.addNew(rawKeys)
		if newCount == 0 {
			dryStreak++
		} else {
			dryStreak = 0
		}
		if dryStreak >= dryLimit {
			fmt.Fprintf(opts.Stdout, "loop: converged — %d consecutive dry round(s) with no new progress; stopping after round %d\n", dryStreak, round)
			break
		}
	}
	if !st.GoalMet {
		fmt.Fprintf(opts.Stdout, "loop: goal not met after %d round(s)\n", st.Round)
	}
	return st, nil
}

// runRound plans, reorganizes, and executes a single round as a pipeline, and
// returns its RoundState plus the convergence keys it surfaced.
func (opts Options) runRound(ctx context.Context, round int, st *state.State) (state.RoundState, []string) {
	started := opts.Now().UTC()
	seats := opts.Planner.Plan(ctx, opts.Goal, st)
	seats = opts.Reorg.Apply(seats, st)
	seats = stampRound(seats, round)
	byRole := indexByRole(seats)

	var results []agent.SeatResult
	// outcomeOf projects the already-run results for the named roles into upstream
	// outcomes — the DATA that flows across a pipeline edge.
	outcomeOf := func(roles ...string) []state.SeatOutcome {
		var out []state.SeatOutcome
		for _, r := range results {
			for _, role := range roles {
				if r.Role == role {
					out = append(out, toOutcome(r))
				}
			}
		}
		return out
	}

	// research → build → critic, strictly sequenced so each consumes its upstream.
	if s, ok := byRole[agent.RoleResearch]; ok {
		results = append(results, opts.runSeat(ctx, s, st, nil))
	}
	if s, ok := byRole[agent.RoleBuild]; ok {
		results = append(results, opts.runSeat(ctx, s, st, outcomeOf(agent.RoleResearch)))
	}
	buildRes, hasBuild := lastByRole(results, agent.RoleBuild)

	// Router: inspect the validated build proposal and route the verification.
	rt := routeSoloCritic
	if hasBuild {
		rt = routeForProposal(buildRes.Proposal)
	}

	if s, ok := byRole[agent.RoleCritic]; ok {
		results = append(results, opts.runSeat(ctx, s, st, outcomeOf(agent.RoleResearch, agent.RoleBuild)))
	}

	// Verifier-on-edge: for the full-audit route with a concrete proposal, audit it
	// before it may be accepted into the result.
	audit := verifierResult{accepted: true}
	if rt == routeFullAudit && hasBuild && strings.TrimSpace(buildRes.Proposal) != "" {
		audit = opts.verifyOnEdge(ctx, round, st, outcomeOf(agent.RoleResearch, agent.RoleBuild))
		results = append(results, audit.seats...)
	}

	if s, ok := byRole[agent.RoleMeasure]; ok {
		results = append(results, opts.runSeat(ctx, s, st, outcomeOf(agent.RoleResearch, agent.RoleBuild, agent.RoleCritic)))
	}
	if s, ok := byRole[agent.RoleSynthesize]; ok {
		results = append(results, opts.runSeat(ctx, s, st, allOutcomes(results)))
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
	verdictText, goalMet := verdictFromSeats(results)
	rs.Verdict = verdictText
	// Goal-met requires the verifier to have accepted the work (when it ran).
	rs.GoalMet = goalMet && audit.accepted
	rs.EndedAt = opts.Now().UTC()
	return rs, rawKeys
}

// runSeat builds the seat's brief and runs it, converting any panic in either the
// Briefer or the Runner into a not-OK SeatResult so one bad seat never crashes the
// process or wedges the round.
func (opts Options) runSeat(ctx context.Context, seat agent.SeatSpec, st *state.State, upstream []state.SeatOutcome) (res agent.SeatResult) {
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
			return opts.runMutatingSeat(ctx, mr, seat, st, upstream)
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
func (opts Options) runMutatingSeat(ctx context.Context, mr agent.MutatingBuildRunner, seat agent.SeatSpec, st *state.State, upstream []state.SeatOutcome) agent.SeatResult {
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
	// Detached teardown: remove the throwaway clone on every path (cleanup is
	// context-free, so a cancelled round still tears it down).
	defer cleanup()

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
			results[i] = opts.runSeat(ctx, seat, st, upstream)
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
