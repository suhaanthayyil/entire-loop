package org

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// Briefer assembles the per-seat brief handed to a worker on stdin. The loop
// depends on this interface so tests can supply a static brief and never shell
// out to `entire graph`/`entire brain`.
type Briefer interface {
	Brief(ctx context.Context, goal string, st *state.State, seat agent.SeatSpec) (string, error)
}

// Options configures a loop run. Runner, Briefer, and Store are required; the
// rest have sensible defaults.
type Options struct {
	Goal     string
	Rounds   int
	Jobs     int
	Model    string
	Effort   string
	RepoRoot string

	Runner  agent.Runner
	Briefer Briefer
	Planner Planner
	Reorg   Reorg
	Store   *state.Store

	Now    func() time.Time
	Stdout io.Writer
}

// Run drives one bounded loop: for each round it plans the seats, applies the
// reorg seam, fans them out concurrently (bounded by Jobs), collects results,
// reads the measure seat's metrics and the critic/synthesize verdict, merges the
// round into persisted state, prints a concise summary, and terminates when the
// goal is met or the round budget is exhausted.
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
	if opts.Rounds <= 0 {
		opts.Rounds = 1
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

	start := st.Round + 1
	for round := start; round <= opts.Rounds; round++ {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		roundState := opts.runRound(ctx, round, st)
		state.Merge(st, roundState)
		if err := opts.Store.Save(st); err != nil {
			return st, err
		}
		printRoundSummary(opts.Stdout, roundState)
		if roundState.GoalMet {
			fmt.Fprintf(opts.Stdout, "loop: goal met after round %d — stopping\n", round)
			break
		}
	}
	if !st.GoalMet {
		fmt.Fprintf(opts.Stdout, "loop: goal not met after %d round(s)\n", st.Round)
	}
	return st, nil
}

// runRound plans and executes a single round, returning its RoundState.
func (opts Options) runRound(ctx context.Context, round int, st *state.State) state.RoundState {
	started := opts.Now().UTC()
	seats := opts.Planner.Plan(opts.Goal, st)
	seats = opts.Reorg.Apply(seats, st)

	results := opts.fanOut(ctx, seats, st)

	rs := state.RoundState{Round: round, StartedAt: started}
	for _, res := range results {
		rs.Seats = append(rs.Seats, toOutcome(res))
		rs.CostUSD += res.CostUSD
	}
	rs.Metrics = metricsFromSeats(results)
	rs.Verdict, rs.GoalMet = verdictFromSeats(results)
	rs.EndedAt = opts.Now().UTC()
	return rs
}

// fanOut runs the seats concurrently, bounded by Jobs, and returns their results
// in the seats' planned order.
func (opts Options) fanOut(ctx context.Context, seats []agent.SeatSpec, st *state.State) []agent.SeatResult {
	results := make([]agent.SeatResult, len(seats))
	sem := make(chan struct{}, opts.Jobs)
	var wg sync.WaitGroup

	for i, seat := range seats {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, seat agent.SeatSpec) {
			defer wg.Done()
			defer func() { <-sem }()

			brief, err := opts.Briefer.Brief(ctx, opts.Goal, st, seat)
			res := opts.Runner.Run(ctx, seat, brief)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("brief degraded: %v", err))
			}
			results[i] = res
		}(i, seat)
	}
	wg.Wait()
	return results
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
	fmt.Fprintf(w, "round %d: seats=%d goal_met=%v cost=$%.4f\n", rs.Round, len(rs.Seats), rs.GoalMet, rs.CostUSD)
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
