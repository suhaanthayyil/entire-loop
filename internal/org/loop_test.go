package org

import (
	"context"
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// staticBriefer returns a fixed brief and never shells out.
type staticBriefer struct{ err error }

func (s staticBriefer) Brief(_ context.Context, _ string, _ *state.State, seat agent.SeatSpec, _ []state.SeatOutcome) (string, error) {
	return "brief for " + seat.Role, s.err
}

func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	return state.NewStore(t.TempDir(), "run-test")
}

func TestRun_TerminatesWhenGoalMet(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleCritic:  {OK: true, GoalMet: true, Verdict: "ship it"},
			agent.RoleMeasure: {OK: true, Metrics: map[string]float64{"progress": 1.0}},
		},
	}
	st, err := Run(context.Background(), Options{
		Goal:    "reach the goal",
		Rounds:  5, // budget of 5, but goal is met in round 1
		Jobs:    2,
		Runner:  fake,
		Briefer: staticBriefer{},
		Store:   newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !st.GoalMet {
		t.Errorf("expected goal met")
	}
	if st.Round != 1 {
		t.Errorf("expected to stop after round 1, got round %d", st.Round)
	}
	if len(st.Rounds) != 1 {
		t.Errorf("recorded rounds = %d, want 1 (early termination)", len(st.Rounds))
	}
	if st.Metrics["progress"] != 1.0 {
		t.Errorf("progress = %v, want 1.0", st.Metrics["progress"])
	}
}

func TestRun_ExhaustsRoundsWhenGoalNotMet(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleCritic: {OK: true, GoalMet: false, Verdict: "not yet"},
		},
	}
	st, err := Run(context.Background(), Options{
		Goal:    "never satisfied",
		Rounds:  3,
		Jobs:    4,
		Runner:  fake,
		Briefer: staticBriefer{},
		Store:   newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.GoalMet {
		t.Errorf("goal should not be met")
	}
	if st.Round != 3 {
		t.Errorf("expected to run all 3 rounds, got round %d", st.Round)
	}
	if len(st.Rounds) != 3 {
		t.Errorf("recorded rounds = %d, want 3", len(st.Rounds))
	}
	// Four seats planned per round → 12 total runner calls across 3 rounds.
	if got := len(fake.Calls()); got != 12 {
		t.Errorf("runner calls = %d, want 12", got)
	}
}

func TestRun_CriticNotOKDoesNotTerminate(t *testing.T) {
	t.Parallel()
	// The critic claims goalMet=true but is not OK (e.g. degraded output) — the
	// loop must NOT terminate on an unreliable verdict.
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleCritic: {OK: false, GoalMet: true, Verdict: "garbled"},
		},
	}
	st, err := Run(context.Background(), Options{
		Goal:    "guard against unreliable verdicts",
		Rounds:  2,
		Runner:  fake,
		Briefer: staticBriefer{},
		Store:   newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.GoalMet {
		t.Errorf("goal-met must require an OK critic; got GoalMet=true")
	}
	if st.Round != 2 {
		t.Errorf("expected all 2 rounds to run, got %d", st.Round)
	}
}

func TestRun_ReorgSeamIsInvoked(t *testing.T) {
	t.Parallel()
	spy := &spyReorg{}
	fake := &agent.FakeRunner{Default: agent.SeatResult{OK: true}}
	_, err := Run(context.Background(), Options{
		Goal:    "seam",
		Rounds:  1,
		Runner:  fake,
		Briefer: staticBriefer{},
		Reorg:   spy,
		Store:   newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !spy.called {
		t.Errorf("expected the reorg seam to be invoked")
	}
}

func TestRun_RequiresRunnerAndBriefer(t *testing.T) {
	t.Parallel()
	if _, err := Run(context.Background(), Options{Goal: "g", Store: newTestStore(t), Briefer: staticBriefer{}}); err == nil {
		t.Errorf("expected error when Runner is nil")
	}
	if _, err := Run(context.Background(), Options{Goal: "g", Store: newTestStore(t), Runner: &agent.FakeRunner{}}); err == nil {
		t.Errorf("expected error when Briefer is nil")
	}
	if _, err := Run(context.Background(), Options{Goal: "", Store: newTestStore(t), Runner: &agent.FakeRunner{}, Briefer: staticBriefer{}}); err == nil {
		t.Errorf("expected error when goal is empty")
	}
}

type spyReorg struct{ called bool }

func (s *spyReorg) Apply(seats []agent.SeatSpec, _ *state.State) []agent.SeatSpec {
	s.called = true
	return seats
}
