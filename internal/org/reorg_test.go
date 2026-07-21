package org

import (
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

func rolesOf(seats []agent.SeatSpec) map[string]bool {
	m := map[string]bool{}
	for _, s := range seats {
		m[s.Role] = true
	}
	return m
}

func TestRulesReorg_SmallGoalCollapsesToSolo(t *testing.T) {
	t.Parallel()
	full := PlanRound("goal", nil)
	// A short goal on the first round collapses to build + critic (implement +
	// verify); research and measure are dropped.
	got := RulesReorg{Goal: "add a flag"}.Apply(full, &state.State{Round: 0})
	roles := rolesOf(got)
	if !roles[agent.RoleBuild] || !roles[agent.RoleCritic] {
		t.Errorf("small goal should keep build+critic; got %v", roles)
	}
	if roles[agent.RoleResearch] || roles[agent.RoleMeasure] {
		t.Errorf("small goal should drop research+measure; got %v", roles)
	}
}

func TestRulesReorg_LargeGoalKeepsFullRoster(t *testing.T) {
	t.Parallel()
	full := PlanRound("goal", nil)
	big := "refactor the entire ingestion pipeline to be fully deterministic across regions and cells"
	got := RulesReorg{Goal: big}.Apply(full, &state.State{Round: 0})
	if len(got) != len(full) {
		t.Errorf("a large goal should keep the full roster; got %d of %d", len(got), len(full))
	}
}

func TestRulesReorg_BudgetOutpacesProgressCollapses(t *testing.T) {
	t.Parallel()
	full := PlanRound("goal", nil)
	// Spent real money, measured zero progress → collapse to the cheap seats.
	st := &state.State{Round: 2, TotalCostUSD: 5.0, Metrics: map[string]float64{"progress": 0}}
	got := RulesReorg{Goal: "a longer multi word goal that is not small at all here"}.Apply(full, st)
	roles := rolesOf(got)
	if !roles[agent.RoleBuild] || !roles[agent.RoleMeasure] {
		t.Errorf("budget>progress should keep build+measure; got %v", roles)
	}
	if roles[agent.RoleResearch] || roles[agent.RoleCritic] {
		t.Errorf("budget>progress should drop the deep research+critic seats; got %v", roles)
	}
}

func TestRulesReorg_HealthyRunKeepsRoster(t *testing.T) {
	t.Parallel()
	full := PlanRound("goal", nil)
	// Good progress for the spend → no collapse.
	st := &state.State{Round: 2, TotalCostUSD: 1.0, Metrics: map[string]float64{"progress": 0.9}}
	got := RulesReorg{Goal: "a longer multi word goal that is not small at all here"}.Apply(full, st)
	if len(got) != len(full) {
		t.Errorf("a healthy run should keep the full roster; got %d of %d", len(got), len(full))
	}
}

func TestNoopReorg_Identity(t *testing.T) {
	t.Parallel()
	full := PlanRound("goal", nil)
	got := NoopReorg{}.Apply(full, nil)
	if len(got) != len(full) {
		t.Errorf("NoopReorg must return the roster unchanged")
	}
}
