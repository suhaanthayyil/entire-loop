package org

import (
	"strings"
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// ---- taxonomy ----------------------------------------------------------------

// TestTaxonomy_CoversEveryEdgeKind: the wired taxonomy has exactly one edge per
// declared kind, each with a non-empty name — the closed set is complete.
func TestTaxonomy_CoversEveryEdgeKind(t *testing.T) {
	t.Parallel()
	edges := Taxonomy()
	if len(edges) != len(AllEdgeKinds) {
		t.Fatalf("Taxonomy has %d edges, want one per kind (%d)", len(edges), len(AllEdgeKinds))
	}
	seen := map[EdgeKind]string{}
	for _, e := range edges {
		if e.Name() == "" {
			t.Errorf("edge of kind %v has an empty name", e.Kind())
		}
		if prev, dup := seen[e.Kind()]; dup {
			t.Errorf("two edges share kind %v (%q and %q)", e.Kind(), prev, e.Name())
		}
		seen[e.Kind()] = e.Name()
	}
	for _, k := range AllEdgeKinds {
		if _, ok := seen[k]; !ok {
			t.Errorf("taxonomy is missing an edge of kind %v", k)
		}
	}
}

// ---- DataEdge ----------------------------------------------------------------

// TestDataEdge_ProjectsNamedUpstream: a DataEdge projects exactly its named
// upstream roles, in results order, and nothing else.
func TestDataEdge_ProjectsNamedUpstream(t *testing.T) {
	t.Parallel()
	results := []agent.SeatResult{
		{Role: agent.RoleResearch, OK: true, Findings: []string{"F"}},
		{Role: agent.RoleBuild, OK: true, Proposal: "DIFF"},
		{Role: agent.RoleMeasure, OK: true, Metrics: map[string]float64{"progress": 1}},
	}

	only := DataEdge{From: []string{agent.RoleResearch}}.Project(results)
	if len(only) != 1 || only[0].Role != agent.RoleResearch || only[0].Findings[0] != "F" {
		t.Fatalf("single-role projection = %+v, want the research outcome", only)
	}

	pair := DataEdge{From: []string{agent.RoleResearch, agent.RoleBuild}}.Project(results)
	if len(pair) != 2 || pair[0].Role != agent.RoleResearch || pair[1].Role != agent.RoleBuild {
		t.Fatalf("two-role projection = %+v, want [research build] in order", pair)
	}
	if pair[1].Proposal != "DIFF" {
		t.Errorf("build proposal not carried across the edge; got %q", pair[1].Proposal)
	}

	none := DataEdge{From: []string{agent.RoleCritic}}.Project(results)
	if len(none) != 0 {
		t.Errorf("projecting an absent role should yield nothing; got %+v", none)
	}
}

// ---- ConditionalEdge (router as an edge) -------------------------------------

// TestConditionalEdge_RoutesFromBuildOutcome: the router edge reads the round's
// build outcome — large → full-audit, small/empty/absent → solo-critic.
func TestConditionalEdge_RoutesFromBuildOutcome(t *testing.T) {
	t.Parallel()
	large := RoundView{Results: []agent.SeatResult{{Role: agent.RoleBuild, Proposal: largeDiff("X")}}}
	if got := (ConditionalEdge{}).Route(large); got != routeFullAudit {
		t.Errorf("large proposal routed to %v, want full-audit", got)
	}

	small := RoundView{Results: []agent.SeatResult{{Role: agent.RoleBuild, Proposal: "--- a/x\n+++ b/x\n+one\n"}}}
	if got := (ConditionalEdge{}).Route(small); got != routeSoloCritic {
		t.Errorf("small proposal routed to %v, want solo-critic", got)
	}

	noBuild := RoundView{Results: []agent.SeatResult{{Role: agent.RoleResearch}}}
	if got := (ConditionalEdge{}).Route(noBuild); got != routeSoloCritic {
		t.Errorf("no build seat should route solo-critic; got %v", got)
	}
}

// ---- CycleEdge (loop-until-dry state machine) --------------------------------

func okResearchRound(findings ...string) state.RoundState {
	return state.RoundState{Seats: []state.SeatOutcome{{Role: agent.RoleResearch, OK: true, Findings: findings}}}
}

// TestCycleEdge_GoalMetStopsImmediately: a goal-met round yields the immediate
// goal-met stop transition, whatever the streak state.
func TestCycleEdge_GoalMetStopsImmediately(t *testing.T) {
	t.Parallel()
	c := newCycleEdge(nil, 2, false)
	dec := c.decide(1, state.RoundState{Round: 1, GoalMet: true, Seats: okResearchRound("F").Seats}, nil)
	if dec.action != cycleStopGoalMet {
		t.Fatalf("goal-met should stop immediately; got action %v", dec.action)
	}
}

// TestCycleEdge_DryStreakConverges: the same finding every round advances the dry
// streak and converges once the limit is hit — never before.
func TestCycleEdge_DryStreakConverges(t *testing.T) {
	t.Parallel()
	c := newCycleEdge(nil, 2, false)
	key := []string{findingKey("STABLE")}
	rs := okResearchRound("STABLE")

	if dec := c.decide(1, rs, key); dec.action != cycleContinue { // new key
		t.Fatalf("round 1 (new finding) should continue; got %v", dec.action)
	}
	if dec := c.decide(2, rs, key); dec.action != cycleContinue { // dry streak 1
		t.Fatalf("round 2 (dry streak 1) should continue; got %v", dec.action)
	}
	dec := c.decide(3, rs, key) // dry streak 2 == limit
	if dec.action != cycleStop {
		t.Fatalf("round 3 (dry streak 2) should stop; got %v", dec.action)
	}
	if dec.message == "" {
		t.Errorf("a convergence stop must carry a log message")
	}
}

// TestCycleEdge_FailStreakIsDistinctFromDry: rounds where no content seat succeeds
// advance a SEPARATE fail streak (reset dry), stop with a distinct reason, and do
// not fold keys into seen.
func TestCycleEdge_FailStreakIsDistinctFromDry(t *testing.T) {
	t.Parallel()
	c := newCycleEdge(nil, 2, false)
	failRound := state.RoundState{Seats: []state.SeatOutcome{{Role: agent.RoleResearch, OK: false}}}

	if dec := c.decide(1, failRound, []string{findingKey("X")}); dec.action != cycleContinue {
		t.Fatalf("fail streak 1 should continue; got %v", dec.action)
	}
	dec := c.decide(2, failRound, []string{findingKey("X")})
	if dec.action != cycleStop {
		t.Fatalf("fail streak 2 should stop; got %v", dec.action)
	}
	if !strings.Contains(dec.message, "no content seat succeeded") {
		t.Errorf("fail stop should carry the distinct failed-round reason; got %q", dec.message)
	}
	// The keys from failed rounds were NOT folded into seen: a later OK round with
	// the same key still counts as new progress (continues, resets nothing to dry).
	if dec := c.decide(3, okResearchRound("X"), []string{findingKey("X")}); dec.action != cycleContinue {
		t.Errorf("the failed rounds' key must not be in seen; round 3 should continue as new, got %v", dec.action)
	}
}

// TestCycleEdge_FixedCountNeverConverges: in fixed-count mode a dry round never
// stops the loop — only the round cap (enforced by the caller) bounds it.
func TestCycleEdge_FixedCountNeverConverges(t *testing.T) {
	t.Parallel()
	c := newCycleEdge(nil, 2, true)
	key := []string{findingKey("STABLE")}
	rs := okResearchRound("STABLE")
	for round := 1; round <= 5; round++ {
		if dec := c.decide(round, rs, key); dec.action != cycleContinue {
			t.Fatalf("fixed-count round %d must continue; got %v", round, dec.action)
		}
	}
}

// TestCycleEdge_RebuildsSeenFromResume: a resumed run's prior-round keys are
// already in seen, so an identical finding reads as dry immediately.
func TestCycleEdge_RebuildsSeenFromResume(t *testing.T) {
	t.Parallel()
	resumed := &state.State{Round: 1, Rounds: []state.RoundState{
		{Round: 1, Seats: []state.SeatOutcome{{Role: agent.RoleResearch, OK: true, Findings: []string{"OLD"}}}},
	}}
	c := newCycleEdge(resumed, 2, false)
	key := []string{findingKey("OLD")}
	rs := okResearchRound("OLD")
	// Round 2 is already dry (streak 1); round 3 converges.
	if dec := c.decide(2, rs, key); dec.action != cycleContinue {
		t.Fatalf("resumed dry round should continue at streak 1; got %v", dec.action)
	}
	if dec := c.decide(3, rs, key); dec.action != cycleStop {
		t.Fatalf("resumed run should converge on the seen key by round 3; got %v", dec.action)
	}
}

// ---- typed metrics -----------------------------------------------------------

// TestRoundMetrics_TypedAccessorsAndRoundTrip: Progress/Risk are first-class,
// arbitrary keys survive, and ToMap is an independent copy.
func TestRoundMetrics_TypedAccessorsAndRoundTrip(t *testing.T) {
	t.Parallel()
	m := metricsFrom(map[string]float64{"progress": 0.75, "risk": 0.2, "coverage": 91})
	if m.Progress() != 0.75 {
		t.Errorf("Progress() = %v, want 0.75", m.Progress())
	}
	if m.Risk() != 0.2 {
		t.Errorf("Risk() = %v, want 0.2", m.Risk())
	}
	if v, ok := m.Get("coverage"); !ok || v != 91 {
		t.Errorf("Get(coverage) = %v,%v, want 91,true", v, ok)
	}
	if _, ok := m.Get("absent"); ok {
		t.Errorf("Get(absent) should report ok=false")
	}
	if m.Len() != 3 {
		t.Errorf("Len() = %d, want 3", m.Len())
	}

	out := m.ToMap()
	out["progress"] = -1 // mutate the copy
	if m.Progress() != 0.75 {
		t.Errorf("ToMap must be an independent copy; Progress changed to %v", m.Progress())
	}

	// A nil map is safe and reads zero.
	zero := metricsFrom(nil)
	if zero.Progress() != 0 || zero.Len() != 0 {
		t.Errorf("nil metrics should be zero-valued; got progress=%v len=%d", zero.Progress(), zero.Len())
	}
}
