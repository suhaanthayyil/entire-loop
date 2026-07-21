// Package org is the control plane of the loop: it plans each round's worker
// seats, runs them as a DATA-CARRYING pipeline (research → build → critic →
// measure/synthesize) with a router and a verifier-on-edge, converges the run
// until it goes dry, and holds the runtime-reorg seam.
package org

import (
	"context"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// Planner decides which worker seats run in the next round. FixedPlanner returns
// a static roster; LLMPlanner spawns a control seat that plans the round from the
// accumulated state and re-plans every round (self-directing graph). The ctx is
// threaded so a planner that runs a worker (LLMPlanner's control seat) honors
// cancellation.
type Planner interface {
	Plan(ctx context.Context, goal string, st *state.State) []agent.SeatSpec
}

// FixedPlanner returns the canonical roster — research, build, critic, measure —
// with the hybrid brain wiring baked in: research and critic are deep (brain MCP
// on), build and measure are cheap (brief-only, MCP off). The rest stay plan-mode
// read-only. The build seat is MUTATING (writes real code in an isolated throwaway
// clone) ONLY when AllowMutating is set — a default run keeps build in plan-mode
// propose-as-text, so `entire loop run` is safe by default. Model/Effort/RepoRoot
// are stamped onto every seat.
type FixedPlanner struct {
	Model    string
	Effort   string
	RepoRoot string
	// AllowMutating opts the build seat into the isolated bypassPermissions clone
	// (default off → plan-mode propose-as-text).
	AllowMutating bool
}

// Plan implements Planner.
func (p FixedPlanner) Plan(_ context.Context, _ string, _ *state.State) []agent.SeatSpec {
	return dedupeByRole([]agent.SeatSpec{
		{Role: agent.RoleResearch, BriefOnly: false, McpBrain: true, Model: p.Model, Effort: p.Effort, RepoRoot: p.RepoRoot},
		{Role: agent.RoleBuild, BriefOnly: true, McpBrain: false, Mutating: p.AllowMutating, Model: p.Model, Effort: p.Effort, RepoRoot: p.RepoRoot},
		{Role: agent.RoleCritic, BriefOnly: false, McpBrain: true, Model: p.Model, Effort: p.Effort, RepoRoot: p.RepoRoot},
		{Role: agent.RoleMeasure, BriefOnly: true, McpBrain: false, Model: p.Model, Effort: p.Effort, RepoRoot: p.RepoRoot},
	})
}

// verifierSpecs returns the three diverse skeptic verifier seats used by the
// verifier-on-edge. Each carries a distinct role (so their caches never collide)
// and a refutation lens. They are cheap, non-mutating, plan-mode seats. Model/
// Effort/RepoRoot are stamped from the planner so they honor the run's overrides.
func (p FixedPlanner) verifierSpecs() []agent.SeatSpec {
	lenses := []struct {
		role string
		lens string
	}{
		{agent.RoleVerifyCorrectness, "CORRECTNESS: does the change do what the goal asks, without logic errors, off-by-ones, or broken edge cases?"},
		{agent.RoleVerifySecurity, "SECURITY: does the change introduce an injection, an unsafe file/process/network operation, a secret leak, or a resource leak?"},
		{agent.RoleVerifyReproduce, "REPRODUCES / ACTUALLY WORKS: if you traced it end-to-end, would it compile and actually exhibit the claimed behavior, or is it aspirational?"},
	}
	specs := make([]agent.SeatSpec, 0, len(lenses))
	for _, l := range lenses {
		specs = append(specs, agent.SeatSpec{
			Role:      l.role,
			BriefOnly: true,
			McpBrain:  false,
			Model:     p.Model,
			Effort:    p.Effort,
			RepoRoot:  p.RepoRoot,
			Lens:      l.lens,
		})
	}
	return specs
}

// dedupeByRole guarantees at most one seat per role in a round. Two seats sharing
// a role would race the same round-scoped cache path and the cache would collapse
// them into one. The planner is the single source of the roster, so uniqueness is
// enforced here; first occurrence of a role wins.
func dedupeByRole(seats []agent.SeatSpec) []agent.SeatSpec {
	seen := make(map[string]struct{}, len(seats))
	out := make([]agent.SeatSpec, 0, len(seats))
	for _, s := range seats {
		if _, ok := seen[s.Role]; ok {
			continue
		}
		seen[s.Role] = struct{}{}
		out = append(out, s)
	}
	return out
}

// stampRound sets the round on every seat so the runner's cache and worktrees are
// round-scoped (round N never replays round N-1).
func stampRound(seats []agent.SeatSpec, round int) []agent.SeatSpec {
	for i := range seats {
		seats[i].Round = round
	}
	return seats
}

// indexByRole maps a roster to role → spec for the pipeline to look up edges.
func indexByRole(seats []agent.SeatSpec) map[string]agent.SeatSpec {
	m := make(map[string]agent.SeatSpec, len(seats))
	for _, s := range seats {
		m[s.Role] = s
	}
	return m
}

// PlanRound is the spec-named convenience entry point: the canonical roster for a
// goal and state, with no model/effort overrides. The loop itself uses a
// configured Planner (see Options.Planner) so model/effort/repo flow through.
func PlanRound(goal string, st *state.State) []agent.SeatSpec {
	return FixedPlanner{}.Plan(context.Background(), goal, st)
}
