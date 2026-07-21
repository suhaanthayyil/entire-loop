package org

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// controlPlanRunner is a mock Runner keyed by role AND round: the CONTROL seat's
// result is chosen by round (so re-planning can be observed), and worker seats
// return canned results by role. It NEVER spawns claude.
type controlPlanRunner struct {
	mu             sync.Mutex
	calls          []agent.SeatSpec
	controlByRound map[int]agent.SeatResult
	workerByRole   map[string]agent.SeatResult
}

func (r *controlPlanRunner) Run(_ context.Context, spec agent.SeatSpec, _ string) agent.SeatResult {
	r.mu.Lock()
	r.calls = append(r.calls, spec)
	r.mu.Unlock()

	if spec.Role == agent.RoleControl {
		if res, ok := r.controlByRound[spec.Round]; ok {
			res.Role = spec.Role
			return res
		}
		return agent.SeatResult{Role: spec.Role, OK: false, Warnings: []string{"no control plan for this round"}}
	}
	if res, ok := r.workerByRole[spec.Role]; ok {
		res.Role = spec.Role
		return res
	}
	return agent.SeatResult{Role: spec.Role, OK: true}
}

func (r *controlPlanRunner) roles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.Role)
	}
	return out
}

func (r *controlPlanRunner) rolesInRound(round int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if c.Round == round {
			out = append(out, c.Role)
		}
	}
	return out
}

func (r *controlPlanRunner) lastSpec(role string) (agent.SeatSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].Role == role {
			return r.calls[i], true
		}
	}
	return agent.SeatSpec{}, false
}

// recordingCtrlBriefer captures each seat's Focus and the refined goal/subgoals in
// the state at brief time, so tests can assert the plan flowed into briefs.
type recordingCtrlBriefer struct {
	mu      sync.Mutex
	focus   map[string]string
	refined map[string]string
	subs    map[string][]string
}

func (b *recordingCtrlBriefer) Brief(_ context.Context, _ string, st *state.State, seat agent.SeatSpec, _ []state.SeatOutcome) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.focus == nil {
		b.focus = map[string]string{}
		b.refined = map[string]string{}
		b.subs = map[string][]string{}
	}
	b.focus[seat.Role] = seat.Focus
	if st != nil {
		b.refined[seat.Role] = st.RefinedGoal
		b.subs[seat.Role] = append([]string(nil), st.Subgoals...)
	}
	return "brief:" + seat.Role + " focus=[" + seat.Focus + "]", nil
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestLLMPlanner_RunsPlannedSeatsWithFocus: a valid control plan → the loop runs
// exactly those seats with the planned models, and each seat's focus is carried
// into its brief.
func TestLLMPlanner_RunsPlannedSeatsWithFocus(t *testing.T) {
	t.Parallel()
	plan := `{"refined_goal":"RG","subgoals":["S1"],"seats":[
		{"role":"research","model":"sonnet","effort":"high","focus":"FOCUS-RESEARCH"},
		{"role":"build","model":"claude-haiku-4-5","focus":"FOCUS-BUILD"}
	],"stop":false}`
	runner := &controlPlanRunner{controlByRound: map[int]agent.SeatResult{1: {OK: true, Raw: plan}}}
	br := &recordingCtrlBriefer{}
	planner := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}, Runner: runner, Briefer: br, Warn: io.Discard}

	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 1, Runner: runner, Briefer: br, Planner: planner, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := runner.roles()
	// Control seat plus exactly the two planned worker seats — no critic/measure/synthesize.
	for _, want := range []string{agent.RoleControl, agent.RoleResearch, agent.RoleBuild} {
		if !contains(got, want) {
			t.Errorf("expected the %q seat to run; ran %v", want, got)
		}
	}
	for _, notWant := range []string{agent.RoleCritic, agent.RoleMeasure, agent.RoleSynthesize} {
		if contains(got, notWant) {
			t.Errorf("did NOT expect the %q seat to run; ran %v", notWant, got)
		}
	}

	// Models: research is deep (sonnet, brain on); build is cheap (haiku, brain off).
	research, _ := runner.lastSpec(agent.RoleResearch)
	if research.Model != agent.ModelDeep || !research.McpBrain {
		t.Errorf("research seat = %+v, want model=%s McpBrain=true", research, agent.ModelDeep)
	}
	build, _ := runner.lastSpec(agent.RoleBuild)
	if build.Model != agent.ModelCheap || build.McpBrain {
		t.Errorf("build seat = %+v, want model=%s McpBrain=false", build, agent.ModelCheap)
	}

	// Focus flowed into each seat's brief.
	if br.focus[agent.RoleResearch] != "FOCUS-RESEARCH" {
		t.Errorf("research focus = %q, want FOCUS-RESEARCH", br.focus[agent.RoleResearch])
	}
	if br.focus[agent.RoleBuild] != "FOCUS-BUILD" {
		t.Errorf("build focus = %q, want FOCUS-BUILD", br.focus[agent.RoleBuild])
	}
}

// TestLLMPlanner_RefinementFlowsIntoBriefs: refined_goal/subgoals from the plan are
// persisted and appear in subsequent worker briefs (but not in the control seat's
// own brief for the round that produced them — it saw the prior refinement).
func TestLLMPlanner_RefinementFlowsIntoBriefs(t *testing.T) {
	t.Parallel()
	plan := `{"refined_goal":"SHARPENED-GOAL","subgoals":["do-x","do-y"],"seats":[
		{"role":"research","focus":"f"}
	]}`
	runner := &controlPlanRunner{controlByRound: map[int]agent.SeatResult{1: {OK: true, Raw: plan}}}
	br := &recordingCtrlBriefer{}
	planner := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}, Runner: runner, Briefer: br, Warn: io.Discard}

	st, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 1, Runner: runner, Briefer: br, Planner: planner, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The control seat is briefed BEFORE it refines, so its brief carries no refinement.
	if br.refined[agent.RoleControl] != "" {
		t.Errorf("control brief refined goal = %q, want empty (refinement happens after)", br.refined[agent.RoleControl])
	}
	// The research seat is briefed AFTER refinement, so its brief carries the refined goal.
	if br.refined[agent.RoleResearch] != "SHARPENED-GOAL" {
		t.Errorf("research brief refined goal = %q, want SHARPENED-GOAL", br.refined[agent.RoleResearch])
	}
	if !contains(br.subs[agent.RoleResearch], "do-x") || !contains(br.subs[agent.RoleResearch], "do-y") {
		t.Errorf("research brief subgoals = %v, want [do-x do-y]", br.subs[agent.RoleResearch])
	}
	// Refinement is persisted for the next round / a resume.
	if st.RefinedGoal != "SHARPENED-GOAL" {
		t.Errorf("persisted RefinedGoal = %q, want SHARPENED-GOAL", st.RefinedGoal)
	}
	if len(st.Subgoals) != 2 {
		t.Errorf("persisted Subgoals = %v, want 2", st.Subgoals)
	}
}

// TestLLMPlanner_RePlansEachRound: the control seat plans round 2 differently from
// round 1, and the loop runs the re-planned seats — dynamic reorg driven by the
// agent, not a fixed rule.
func TestLLMPlanner_RePlansEachRound(t *testing.T) {
	t.Parallel()
	round1 := `{"refined_goal":"r1","seats":[{"role":"research","focus":"map it"}]}`
	round2 := `{"refined_goal":"r2","seats":[{"role":"build","focus":"now build it"}]}`
	runner := &controlPlanRunner{controlByRound: map[int]agent.SeatResult{
		1: {OK: true, Raw: round1},
		2: {OK: true, Raw: round2},
	}}
	br := &recordingCtrlBriefer{}
	planner := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}, Runner: runner, Briefer: br, Warn: io.Discard}

	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 2, Runner: runner, Briefer: br, Planner: planner, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	r1 := runner.rolesInRound(1)
	if !contains(r1, agent.RoleResearch) || contains(r1, agent.RoleBuild) {
		t.Errorf("round 1 ran %v, want research (not build)", r1)
	}
	r2 := runner.rolesInRound(2)
	if !contains(r2, agent.RoleBuild) || contains(r2, agent.RoleResearch) {
		t.Errorf("round 2 ran %v, want build (not research)", r2)
	}
}

// TestLLMPlanner_ControlSeatIsNonMutatingDeep asserts the control seat itself is
// always a non-mutating, deep-tier plan-mode seat, whatever the config.
func TestLLMPlanner_ControlSeatIsNonMutatingDeep(t *testing.T) {
	t.Parallel()
	plan := `{"seats":[{"role":"build"}]}`
	runner := &controlPlanRunner{controlByRound: map[int]agent.SeatResult{1: {OK: true, Raw: plan}}}
	planner := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r", AllowMutating: true}, Runner: runner, Briefer: staticBriefer{}, Warn: io.Discard}
	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 1, Runner: runner, Briefer: staticBriefer{}, Planner: planner, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ctrl, ok := runner.lastSpec(agent.RoleControl)
	if !ok {
		t.Fatal("control seat never ran")
	}
	if ctrl.Mutating {
		t.Error("the control seat must NEVER be mutating, even with --allow-mutating-build on")
	}
	if ctrl.Model != agent.ModelDeep {
		t.Errorf("control seat model = %q, want the deep tier %q", ctrl.Model, agent.ModelDeep)
	}
}

// ---- guardrail unit tests (mutation-verified via direct sanitize) ----

func TestLLMPlanner_Sanitize_DropsUnknownAndPrivilegedRoles(t *testing.T) {
	t.Parallel()
	p := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}}
	seats := p.sanitize(controlPlan{Seats: []controlSeat{
		{Role: "root"},            // privileged-sounding
		{Role: "build-bypass"},    // privileged-sounding
		{Role: "admin"},           // unknown
		{Role: "verify-security"}, // verifier — not plannable
		{Role: "control"},         // the planner itself — not plannable
		{Role: "research"},        // the only allowed one here
	}})
	if len(seats) != 1 || seats[0].Role != agent.RoleResearch {
		t.Fatalf("sanitize kept %+v, want only the research seat", seats)
	}
}

func TestLLMPlanner_Sanitize_DefaultsDisallowedModelPerTier(t *testing.T) {
	t.Parallel()
	p := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}}
	seats := p.sanitize(controlPlan{Seats: []controlSeat{
		{Role: "research", Model: "opus-ultra"},          // deep tier → sonnet
		{Role: "build", Model: "definitely-not-a-model"}, // cheap tier → haiku
		{Role: "critic", Model: "sonnet"},                // allowlisted → kept
	}})
	byRole := map[string]agent.SeatSpec{}
	for _, s := range seats {
		byRole[s.Role] = s
	}
	if byRole[agent.RoleResearch].Model != agent.ModelDeep {
		t.Errorf("research model = %q, want defaulted to %s", byRole[agent.RoleResearch].Model, agent.ModelDeep)
	}
	if byRole[agent.RoleBuild].Model != agent.ModelCheap {
		t.Errorf("build model = %q, want defaulted to %s", byRole[agent.RoleBuild].Model, agent.ModelCheap)
	}
	if byRole[agent.RoleCritic].Model != agent.ModelDeep {
		t.Errorf("critic model = %q, want kept %s", byRole[agent.RoleCritic].Model, agent.ModelDeep)
	}
}

func TestLLMPlanner_Sanitize_CapsSeatsPerRound(t *testing.T) {
	t.Parallel()
	// Twenty seats cycling through the allowed roles, with a low cap.
	roles := []string{"research", "build", "critic", "measure", "synthesize"}
	var seats []controlSeat
	for i := 0; i < 20; i++ {
		seats = append(seats, controlSeat{Role: roles[i%len(roles)]})
	}
	p := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}, MaxSeats: 3}
	got := p.sanitize(controlPlan{Seats: seats})
	if len(got) != 3 {
		t.Fatalf("sanitize returned %d seats, want the cap of 3 (%v)", len(got), got)
	}
	// Default cap never exceeds maxControlSeatsPerRound either.
	pd := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}}
	if n := len(pd.sanitize(controlPlan{Seats: seats})); n > maxControlSeatsPerRound {
		t.Errorf("default cap returned %d seats, want <= %d", n, maxControlSeatsPerRound)
	}
}

// TestLLMPlanner_MutatingLock is the privilege-lock guard: Mutating is derived
// SOLELY from the human flag AND role==build — never from the plan. A plan asking
// for a mutating build with the flag OFF still yields Mutating=false; unknown
// "privileged" roles are dropped entirely; only with the flag ON may the build seat
// be mutating, and even then only the build seat.
func TestLLMPlanner_MutatingLock(t *testing.T) {
	t.Parallel()
	planWithBuild := controlPlan{Seats: []controlSeat{
		{Role: "build"},
		{Role: "research"},
		{Role: "critic"},
	}}

	// Flag OFF: nothing is mutating, even though the plan wants a build seat.
	off := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r", AllowMutating: false}}
	for _, s := range off.sanitize(planWithBuild) {
		if s.Mutating {
			t.Errorf("flag OFF: seat %q is mutating, want non-mutating", s.Role)
		}
	}

	// A plan trying to name a privileged/mutating role gets that role DROPPED, and
	// the surviving build seat is still non-mutating with the flag off.
	escalation := controlPlan{Seats: []controlSeat{
		{Role: "build-bypass"},
		{Role: "root"},
		{Role: "build"},
	}}
	esc := off.sanitize(escalation)
	if len(esc) != 1 || esc[0].Role != agent.RoleBuild || esc[0].Mutating {
		t.Fatalf("escalation attempt produced %+v, want a single non-mutating build seat", esc)
	}

	// Flag ON: the build seat MAY be mutating; every other seat stays non-mutating.
	on := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r", AllowMutating: true}}
	var sawBuild bool
	for _, s := range on.sanitize(planWithBuild) {
		if s.Role == agent.RoleBuild {
			sawBuild = true
			if !s.Mutating {
				t.Errorf("flag ON: build seat must be mutating; got %+v", s)
			}
			continue
		}
		if s.Mutating {
			t.Errorf("flag ON: only the build seat may be mutating; %q is mutating", s.Role)
		}
	}
	if !sawBuild {
		t.Fatal("build seat missing under flag ON")
	}
}

// ---- graceful-degrade fallback tests ----

func TestLLMPlanner_FallsBackOnGarbledPlan(t *testing.T) {
	t.Parallel()
	var warn bytes.Buffer
	runner := &controlPlanRunner{controlByRound: map[int]agent.SeatResult{1: {OK: true, Raw: "this is not json at all"}}}
	planner := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}, Runner: runner, Briefer: staticBriefer{}, Warn: &warn}

	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 1, Runner: runner, Briefer: staticBriefer{}, Planner: planner, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertFellBackToFixed(t, runner, warn.String())
}

func TestLLMPlanner_FallsBackOnEmptyPlan(t *testing.T) {
	t.Parallel()
	var warn bytes.Buffer
	runner := &controlPlanRunner{controlByRound: map[int]agent.SeatResult{1: {OK: true, Raw: `{"refined_goal":"x","seats":[]}`}}}
	planner := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}, Runner: runner, Briefer: staticBriefer{}, Warn: &warn}

	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 1, Runner: runner, Briefer: staticBriefer{}, Planner: planner, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertFellBackToFixed(t, runner, warn.String())
}

func TestLLMPlanner_FallsBackWhenAllRolesInvalid(t *testing.T) {
	t.Parallel()
	var warn bytes.Buffer
	runner := &controlPlanRunner{controlByRound: map[int]agent.SeatResult{1: {OK: true, Raw: `{"seats":[{"role":"root"},{"role":"nope"}]}`}}}
	planner := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}, Runner: runner, Briefer: staticBriefer{}, Warn: &warn}

	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 1, Runner: runner, Briefer: staticBriefer{}, Planner: planner, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertFellBackToFixed(t, runner, warn.String())
}

func TestLLMPlanner_FallsBackWhenControlSeatDegrades(t *testing.T) {
	t.Parallel()
	var warn bytes.Buffer
	// Control seat returns not-OK (e.g. worker failure / no-egress refusal).
	runner := &controlPlanRunner{controlByRound: map[int]agent.SeatResult{1: {OK: false, Warnings: []string{"worker failed"}}}}
	planner := LLMPlanner{Base: FixedPlanner{RepoRoot: "/r"}, Runner: runner, Briefer: staticBriefer{}, Warn: &warn}

	_, err := Run(context.Background(), Options{
		Goal: "g", Rounds: 1, Runner: runner, Briefer: staticBriefer{}, Planner: planner, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertFellBackToFixed(t, runner, warn.String())
}

// assertFellBackToFixed checks the warning was emitted and the fixed roster ran.
func assertFellBackToFixed(t *testing.T, runner *controlPlanRunner, warn string) {
	t.Helper()
	if !strings.Contains(warn, "falling back") {
		t.Errorf("expected a graceful-degrade warning; got %q", warn)
	}
	got := runner.roles()
	// The fixed roster is research/build/critic/measure — all four must have run.
	for _, want := range []string{agent.RoleResearch, agent.RoleBuild, agent.RoleCritic, agent.RoleMeasure} {
		if !contains(got, want) {
			t.Errorf("fallback should run the fixed roster; %q missing from %v", want, got)
		}
	}
}
