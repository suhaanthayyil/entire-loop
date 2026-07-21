package org

import (
	"strings"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// Reorg is the runtime-reorganization seam. Before a round fans out, the loop
// gives the planned seats to Reorg.Apply, which may adjust the roster based on how
// prior rounds went.
type Reorg interface {
	Apply(seats []agent.SeatSpec, st *state.State) []agent.SeatSpec
}

// NoopReorg returns the planned seats unchanged. It keeps the seam live for tests
// and for runs that want the full roster every round.
type NoopReorg struct{}

// Apply implements Reorg by returning the seats unchanged.
func (NoopReorg) Apply(seats []agent.SeatSpec, _ *state.State) []agent.SeatSpec {
	return seats
}

// smallGoalWords is the word count at or below which a goal is treated as small
// and eligible for the small→solo collapse.
const smallGoalWords = 6

// RulesReorg folds two of the documented reorg rules into real logic:
//
//   - small→solo: a small, well-scoped goal (first round only) collapses to a
//     lean soloist pair — implement (build) + verify (critic) — instead of the
//     full research/build/critic/measure roster. There is nothing to research for
//     a one-line change, so the deep research/measure seats are dropped.
//   - budget>progress→collapse: when prior spend has outpaced measured progress,
//     collapse toward the cheapest seats that can still advance the goal (drop the
//     deep, expensive seats; keep build + measure).
//
// The remaining documented rules (fail-cluster→+critic, 2×fix→promote) stay
// unimplemented; this is a strict, deterministic subset. Rules are checked in
// priority order and at most one fires.
type RulesReorg struct {
	Goal string
	// BudgetPerProgress is the cost-per-progress ratio above which the budget rule
	// fires. Zero uses the default.
	BudgetPerProgress float64
}

const defaultBudgetPerProgress = 2.0

// Apply implements Reorg.
func (r RulesReorg) Apply(seats []agent.SeatSpec, st *state.State) []agent.SeatSpec {
	if len(seats) == 0 {
		return seats
	}
	byRole := indexByRole(seats)

	// budget>progress→collapse takes priority: if we are already burning budget
	// without progress, shed the expensive seats regardless of goal size.
	if r.budgetOutpacesProgress(st) {
		return keepRoles(seats, byRole, agent.RoleBuild, agent.RoleMeasure)
	}

	// small→solo, first round only (there is no prior work to build on yet).
	if goalIsSmall(r.Goal) && (st == nil || st.Round == 0) {
		return keepRoles(seats, byRole, agent.RoleBuild, agent.RoleCritic)
	}

	return seats
}

// budgetOutpacesProgress reports whether accumulated spend has outrun measured
// progress. Progress near zero with any real spend trips it.
func (r RulesReorg) budgetOutpacesProgress(st *state.State) bool {
	if st == nil || st.TotalCostUSD <= 0 {
		return false
	}
	progress := st.Metrics["progress"]
	threshold := r.BudgetPerProgress
	if threshold <= 0 {
		threshold = defaultBudgetPerProgress
	}
	if progress <= 0 {
		// Spent money, measured no progress at all.
		return true
	}
	return st.TotalCostUSD/progress >= threshold
}

// keepRoles returns the subset of seats whose roles are in want, preserving the
// planned order. Roles that are not present in the plan are silently skipped.
func keepRoles(seats []agent.SeatSpec, byRole map[string]agent.SeatSpec, want ...string) []agent.SeatSpec {
	keep := make(map[string]struct{}, len(want))
	for _, role := range want {
		if _, ok := byRole[role]; ok {
			keep[role] = struct{}{}
		}
	}
	if len(keep) == 0 {
		return seats // never collapse to nothing
	}
	out := make([]agent.SeatSpec, 0, len(keep))
	for _, s := range seats {
		if _, ok := keep[s.Role]; ok {
			out = append(out, s)
		}
	}
	return out
}

// goalIsSmall reports whether a goal is short enough to treat as a one-liner.
func goalIsSmall(goal string) bool {
	n := len(strings.Fields(goal))
	return n > 0 && n <= smallGoalWords
}
