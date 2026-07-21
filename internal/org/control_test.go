package org

import (
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
)

func TestFixedPlanner_HybridWiring(t *testing.T) {
	t.Parallel()
	seats := FixedPlanner{Model: "sonnet", Effort: "low", RepoRoot: "/repo"}.Plan("goal", nil)

	byRole := map[string]agent.SeatSpec{}
	for _, s := range seats {
		byRole[s.Role] = s
	}

	// The MVP plan is exactly these four seats.
	for _, role := range []string{agent.RoleResearch, agent.RoleBuild, agent.RoleCritic, agent.RoleMeasure} {
		if _, ok := byRole[role]; !ok {
			t.Fatalf("plan missing seat %q; got %v", role, seats)
		}
	}
	if len(seats) != 4 {
		t.Fatalf("plan has %d seats, want 4", len(seats))
	}

	// Deep seats bind the brain; cheap seats are brief-only with MCP off.
	deep := []string{agent.RoleResearch, agent.RoleCritic}
	cheap := []string{agent.RoleBuild, agent.RoleMeasure}
	for _, role := range deep {
		s := byRole[role]
		if !s.McpBrain || s.BriefOnly {
			t.Errorf("%s should be deep (McpBrain=true, BriefOnly=false); got %+v", role, s)
		}
	}
	for _, role := range cheap {
		s := byRole[role]
		if s.McpBrain || !s.BriefOnly {
			t.Errorf("%s should be cheap (McpBrain=false, BriefOnly=true); got %+v", role, s)
		}
	}

	// Model/Effort/RepoRoot are stamped onto every seat.
	for _, s := range seats {
		if s.Model != "sonnet" || s.Effort != "low" || s.RepoRoot != "/repo" {
			t.Errorf("seat %s missing config stamp; got %+v", s.Role, s)
		}
	}
}

func TestPlanRound_Convenience(t *testing.T) {
	t.Parallel()
	seats := PlanRound("goal", nil)
	if len(seats) != 4 {
		t.Fatalf("PlanRound seats = %d, want 4", len(seats))
	}
}

func TestFixedPlanner_RolesAreUnique(t *testing.T) {
	t.Parallel()
	// Two seats sharing a role would race the same seats/<role>.json cache path;
	// the planner must never emit duplicates.
	seen := map[string]bool{}
	for _, s := range PlanRound("goal", nil) {
		if seen[s.Role] {
			t.Fatalf("duplicate seat role %q in plan", s.Role)
		}
		seen[s.Role] = true
	}
}

func TestDedupeByRole(t *testing.T) {
	t.Parallel()
	in := []agent.SeatSpec{
		{Role: agent.RoleResearch, Model: "first"},
		{Role: agent.RoleBuild},
		{Role: agent.RoleResearch, Model: "second"}, // duplicate role
	}
	out := dedupeByRole(in)
	if len(out) != 2 {
		t.Fatalf("dedupeByRole len = %d, want 2 (%+v)", len(out), out)
	}
	if out[0].Role != agent.RoleResearch || out[0].Model != "first" {
		t.Errorf("first occurrence should win; got %+v", out[0])
	}
	if out[1].Role != agent.RoleBuild {
		t.Errorf("second seat = %+v, want build", out[1])
	}
}
