// Package org is the control plane of the loop: it plans each round's worker
// seats, runs the fan-out/verify/synthesize cycle, and holds the runtime-reorg
// seam. The MVP plan is fixed; the Planner interface is the seam where Phase B
// swaps in an LLM control seat that emits the plan as JSON (see control.md).
package org

import (
	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// Planner decides which worker seats run in the next round. The MVP uses
// FixedPlanner; Phase B implements this interface with a control seat that reads
// the goal + state and returns the plan as JSON.
type Planner interface {
	Plan(goal string, st *state.State) []agent.SeatSpec
}

// FixedPlanner returns the MVP plan — research, build, critic, measure — with the
// hybrid brain wiring baked in: research and critic are deep (brain MCP on),
// build and measure are cheap (brief-only, MCP off). Model/Effort/RepoRoot are
// stamped onto every seat.
type FixedPlanner struct {
	Model    string
	Effort   string
	RepoRoot string
}

// Plan implements Planner.
func (p FixedPlanner) Plan(_ string, _ *state.State) []agent.SeatSpec {
	return dedupeByRole([]agent.SeatSpec{
		{Role: agent.RoleResearch, BriefOnly: false, McpBrain: true, Model: p.Model, Effort: p.Effort, RepoRoot: p.RepoRoot},
		{Role: agent.RoleBuild, BriefOnly: true, McpBrain: false, Model: p.Model, Effort: p.Effort, RepoRoot: p.RepoRoot},
		{Role: agent.RoleCritic, BriefOnly: false, McpBrain: true, Model: p.Model, Effort: p.Effort, RepoRoot: p.RepoRoot},
		{Role: agent.RoleMeasure, BriefOnly: true, McpBrain: false, Model: p.Model, Effort: p.Effort, RepoRoot: p.RepoRoot},
	})
}

// dedupeByRole guarantees at most one seat per role in a round. Two seats sharing
// a role would race the same idempotent-skip cache path (seats/<role>.json) and
// the cache would collapse them into one. The planner is the single source of the
// roster, so uniqueness is enforced here; first occurrence of a role wins.
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

// PlanRound is the spec-named convenience entry point: the MVP fixed plan for a
// goal and state, with no model/effort overrides. The loop itself uses a
// configured Planner (see Options.Planner) so model/effort/repo flow through.
func PlanRound(goal string, st *state.State) []agent.SeatSpec {
	return FixedPlanner{}.Plan(goal, st)
}
