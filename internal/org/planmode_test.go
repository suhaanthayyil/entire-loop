package org

import (
	"context"
	"sync"
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// roundVaryingPlanner re-plans by round: research on round 1, build afterwards. It
// lets a test observe whether the loop re-planned (dynamic) or froze (immutable).
type roundVaryingPlanner struct{}

func (roundVaryingPlanner) Plan(_ context.Context, _ string, st *state.State) []agent.SeatSpec {
	round := 1
	if st != nil {
		round = st.Round + 1
	}
	if round == 1 {
		return []agent.SeatSpec{{Role: agent.RoleResearch}}
	}
	return []agent.SeatSpec{{Role: agent.RoleBuild}}
}

// roleRecordingRunner records which roles ran in which round. It never spawns claude.
type roleRecordingRunner struct {
	mu      sync.Mutex
	byRound map[int][]string
}

func (r *roleRecordingRunner) Run(_ context.Context, spec agent.SeatSpec, _ string) agent.SeatResult {
	r.mu.Lock()
	if r.byRound == nil {
		r.byRound = map[int][]string{}
	}
	r.byRound[spec.Round] = append(r.byRound[spec.Round], spec.Role)
	r.mu.Unlock()
	return agent.SeatResult{Role: spec.Role, OK: true}
}

func (r *roleRecordingRunner) rolesIn(round int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.byRound[round]...)
}

// TestPlanMode_DynamicRePlansEachRound: the default dynamic mode re-plans the DAG
// each round — round 2 runs the re-planned build seat, not round 1's research.
func TestPlanMode_DynamicRePlansEachRound(t *testing.T) {
	t.Parallel()
	runner := &roleRecordingRunner{}
	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 2, Runner: runner, Briefer: staticBriefer{},
		Planner: roundVaryingPlanner{}, Store: newTestStore(t), PlanMode: PlanModeDynamic,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r1 := runner.rolesIn(1); !contains(r1, agent.RoleResearch) || contains(r1, agent.RoleBuild) {
		t.Errorf("round 1 ran %v, want research (not build)", r1)
	}
	if r2 := runner.rolesIn(2); !contains(r2, agent.RoleBuild) || contains(r2, agent.RoleResearch) {
		t.Errorf("dynamic round 2 must re-plan to build; ran %v", r2)
	}
}

// TestPlanMode_ImmutableFreezesDAG: immutable mode plans ONCE up front — round 2
// runs the SAME roster as round 1 even though the planner would have re-planned.
func TestPlanMode_ImmutableFreezesDAG(t *testing.T) {
	t.Parallel()
	runner := &roleRecordingRunner{}
	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 2, Runner: runner, Briefer: staticBriefer{},
		Planner: roundVaryingPlanner{}, Store: newTestStore(t), PlanMode: PlanModeImmutable,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r1 := runner.rolesIn(1); !contains(r1, agent.RoleResearch) {
		t.Errorf("round 1 should run research; ran %v", r1)
	}
	// Frozen: round 2 is the SAME research roster, NOT the planner's round-2 build.
	if r2 := runner.rolesIn(2); !contains(r2, agent.RoleResearch) || contains(r2, agent.RoleBuild) {
		t.Errorf("immutable round 2 must reuse the frozen research roster; ran %v", r2)
	}
}

// TestPlanMode_ImmutableResumeReusesRoster is the finding-4 guard: the immutable
// DAG is planned ONCE and persisted, so a RESUME (a fresh process → fresh in-memory
// frozen roster) rehydrates the SAME roster from state instead of re-planning a
// different one. roundVaryingPlanner would plan build from round 2 on; the resumed
// round 2 must still run the frozen research roster.
func TestPlanMode_ImmutableResumeReusesRoster(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	// First process: round 1 freezes the research roster and persists it.
	r1 := &roleRecordingRunner{}
	if _, err := Run(context.Background(), Options{
		Goal: "freeze then resume", Rounds: 1, Runner: r1, Briefer: staticBriefer{},
		Planner: roundVaryingPlanner{}, Store: store, PlanMode: PlanModeImmutable,
	}); err != nil {
		t.Fatalf("Run(round 1): %v", err)
	}
	if r := r1.rolesIn(1); !contains(r, agent.RoleResearch) || contains(r, agent.RoleBuild) {
		t.Fatalf("round 1 should freeze the research roster; ran %v", r)
	}

	// Resume: a fresh runner and a fresh frozenRoster, same store. The roster must
	// rehydrate from persisted state, NOT the planner's round-2 build.
	r2 := &roleRecordingRunner{}
	st, err := Run(context.Background(), Options{
		Goal: "freeze then resume", Rounds: 2, Runner: r2, Briefer: staticBriefer{},
		Planner: roundVaryingPlanner{}, Store: store, PlanMode: PlanModeImmutable,
	})
	if err != nil {
		t.Fatalf("Run(resume): %v", err)
	}
	if st.Round != 2 {
		t.Fatalf("resume should advance to round 2; got %d", st.Round)
	}
	if r := r2.rolesIn(2); !contains(r, agent.RoleResearch) || contains(r, agent.RoleBuild) {
		t.Errorf("resumed immutable round 2 must reuse the frozen research roster; ran %v", r)
	}
}

// flakyRunner forces a bounded number of failures per role, then succeeds. It lets
// a test observe per-node recovery (retry) vs no recovery.
type flakyRunner struct {
	mu        sync.Mutex
	failsLeft map[string]int
	calls     map[string]int
}

func (r *flakyRunner) Run(_ context.Context, spec agent.SeatSpec, _ string) agent.SeatResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[spec.Role]++
	if r.failsLeft[spec.Role] > 0 {
		r.failsLeft[spec.Role]--
		return agent.SeatResult{Role: spec.Role, OK: false, Warnings: []string{"transient failure"}}
	}
	return agent.SeatResult{Role: spec.Role, OK: true}
}

func (r *flakyRunner) callCount(role string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[role]
}

// TestPlanMode_ImmutableRecoversFailedNode: in immutable mode a node that fails
// once is retried (bounded) and recovers to OK; the retry warning is recorded.
func TestPlanMode_ImmutableRecoversFailedNode(t *testing.T) {
	t.Parallel()
	runner := &flakyRunner{failsLeft: map[string]int{agent.RoleBuild: 1}}
	st, err := Run(context.Background(), Options{
		Goal: "recover a flaky node", Rounds: 1, Runner: runner, Briefer: staticBriefer{},
		Planner: FixedPlanner{}, Store: newTestStore(t), PlanMode: PlanModeImmutable,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.callCount(agent.RoleBuild); got != 2 {
		t.Errorf("build node should be retried once (2 calls); got %d", got)
	}
	build, ok := outcomeWith(st.Rounds[0].Seats, agent.RoleBuild)
	if !ok || !build.OK {
		t.Fatalf("build node should recover to OK; got %+v ok=%v", build, ok)
	}
	if !hasSubstr(build.Warnings, "retried") {
		t.Errorf("a recovered node should carry a retry warning; got %v", build.Warnings)
	}
}

// TestPlanMode_DynamicDoesNotRetry: the default dynamic mode does NOT retry a
// failed node (re-planning is its recovery), so a one-shot failure stays degraded.
func TestPlanMode_DynamicDoesNotRetry(t *testing.T) {
	t.Parallel()
	runner := &flakyRunner{failsLeft: map[string]int{agent.RoleBuild: 1}}
	st, err := Run(context.Background(), Options{
		Goal: "no retry in dynamic mode", Rounds: 1, Runner: runner, Briefer: staticBriefer{},
		Planner: FixedPlanner{}, Store: newTestStore(t), PlanMode: PlanModeDynamic,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.callCount(agent.RoleBuild); got != 1 {
		t.Errorf("dynamic mode must not retry; build calls = %d, want 1", got)
	}
	build, _ := outcomeWith(st.Rounds[0].Seats, agent.RoleBuild)
	if build.OK {
		t.Errorf("a failed node in dynamic mode stays degraded; got OK build")
	}
}

// TestPlanMode_ImmutableDisablesRuntimeReorg: immutable mode disables the runtime
// reorg (a dynamic mechanism). A small goal that dynamic mode would collapse to
// build+critic runs the FULL frozen roster under immutable mode.
func TestPlanMode_ImmutableDisablesRuntimeReorg(t *testing.T) {
	t.Parallel()
	small := "add a flag" // <= smallGoalWords → dynamic reorg collapses round 1

	dyn := &roleRecordingRunner{}
	if _, err := Run(context.Background(), Options{
		Goal: small, Rounds: 1, Runner: dyn, Briefer: staticBriefer{},
		Planner: FixedPlanner{}, Reorg: RulesReorg{Goal: small}, Store: newTestStore(t), PlanMode: PlanModeDynamic,
	}); err != nil {
		t.Fatalf("Run(dynamic): %v", err)
	}
	if r1 := dyn.rolesIn(1); contains(r1, agent.RoleResearch) || contains(r1, agent.RoleMeasure) {
		t.Errorf("dynamic small-goal reorg should drop research/measure; ran %v", r1)
	}

	imm := &roleRecordingRunner{}
	if _, err := Run(context.Background(), Options{
		Goal: small, Rounds: 1, Runner: imm, Briefer: staticBriefer{},
		Planner: FixedPlanner{}, Reorg: RulesReorg{Goal: small}, Store: newTestStore(t), PlanMode: PlanModeImmutable,
	}); err != nil {
		t.Fatalf("Run(immutable): %v", err)
	}
	// Reorg disabled → the full research/build/critic/measure roster runs.
	for _, role := range []string{agent.RoleResearch, agent.RoleBuild, agent.RoleCritic, agent.RoleMeasure} {
		if !contains(imm.rolesIn(1), role) {
			t.Errorf("immutable mode should keep the full roster (reorg off); %q missing from %v", role, imm.rolesIn(1))
		}
	}
}
