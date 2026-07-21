package org

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// perRoundRunner returns a research finding that is UNIQUE per round (so the run
// never goes dry) and never meets the goal — used to exercise the max-rounds cap.
type perRoundRunner struct{}

func (perRoundRunner) Run(_ context.Context, spec agent.SeatSpec, _ string) agent.SeatResult {
	res := agent.SeatResult{Role: spec.Role, OK: true}
	if spec.Role == agent.RoleResearch {
		res.Findings = []string{fmt.Sprintf("finding-round-%d", spec.Round)}
	}
	return res
}

// TestConverge_StopsOnDryStreak: a runner that surfaces the SAME finding every
// round goes dry after the first round; the run stops once the dry streak is hit,
// well before the max-rounds cap.
func TestConverge_StopsOnDryStreak(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleResearch: {OK: true, Findings: []string{"STABLE-FINDING"}},
			agent.RoleCritic:   {OK: true, GoalMet: false},
		},
	}
	st, err := Run(context.Background(), Options{
		Goal: "converge on a dry streak", Rounds: 0, MaxRounds: 10, DryStreakLimit: 2,
		Runner: fake, Briefer: staticBriefer{}, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// round 1: new finding; round 2: dry (streak 1); round 3: dry (streak 2 → stop).
	if st.Round != 3 {
		t.Errorf("expected to converge after 3 rounds (dry streak), got %d", st.Round)
	}
	if st.Round >= 10 {
		t.Errorf("must stop well before the max-rounds cap")
	}
}

// TestConverge_RejectedItemDoesNotResurface: a large proposal is refuted every
// round and dropped, but the SAME proposal is deduped against `seen`, so the run
// still goes dry and converges instead of looping forever on the rejected item.
func TestConverge_RejectedItemDoesNotResurface(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleResearch:          {OK: true, Findings: []string{"STABLE-FINDING"}},
			agent.RoleBuild:             {OK: true, Proposal: largeDiff("REJECTED")},
			agent.RoleCritic:            {OK: true, GoalMet: false},
			agent.RoleVerifyCorrectness: {OK: true, GoalMet: false},
			agent.RoleVerifySecurity:    {OK: true, GoalMet: false},
			agent.RoleVerifyReproduce:   {OK: true, GoalMet: false},
		},
	}
	st, err := Run(context.Background(), Options{
		Goal: "rejected item must not resurface forever", Rounds: 0, MaxRounds: 10, DryStreakLimit: 2,
		Runner: fake, Briefer: staticBriefer{}, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.Round != 3 {
		t.Errorf("a repeatedly-rejected proposal is in `seen`, so the run should converge after 3 rounds; got %d", st.Round)
	}
	// Every round dropped the refuted proposal.
	for _, rd := range st.Rounds {
		if build, ok := outcomeWith(rd.Seats, agent.RoleBuild); ok && build.Proposal != "" {
			t.Errorf("round %d: refuted proposal should have been dropped", rd.Round)
		}
	}
}

// TestConverge_MaxRoundsCap: a run that surfaces new progress every round (never
// dry) and never meets the goal must stop at the max-rounds safety cap.
func TestConverge_MaxRoundsCap(t *testing.T) {
	t.Parallel()
	st, err := Run(context.Background(), Options{
		Goal: "never dry, cap must apply", Rounds: 0, MaxRounds: 3, DryStreakLimit: 2,
		Runner: perRoundRunner{}, Briefer: staticBriefer{}, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.Round != 3 {
		t.Errorf("max-rounds cap = 3 should stop at round 3; got %d", st.Round)
	}
	if st.GoalMet {
		t.Errorf("goal was never met")
	}
}

// TestNoEgress_RoundRefusalIsError: a round entirely refused under the no-egress
// gate surfaces as an error so scripts see a non-zero exit.
func TestNoEgress_RoundRefusalIsError(t *testing.T) {
	t.Parallel()
	refuse := &agent.FakeRunner{
		Default: agent.SeatResult{OK: false, Warnings: []string{"no_egress: worker agent \"claude\" can send context off loopback"}},
	}
	_, err := Run(context.Background(), Options{
		Goal: "refused under no-egress", Rounds: 1,
		Runner: refuse, Briefer: staticBriefer{}, Store: newTestStore(t),
	})
	if err == nil {
		t.Fatalf("a fully-refused no-egress round must return an error (non-zero exit)")
	}
}

// perRoundVerifierRunner surfaces STABLE research findings/proposal every round
// (so the content is dry) but emits round-VARYING verifier and measure output.
// Those must NOT count as progress, else the loop never converges.
type perRoundVerifierRunner struct{}

func (perRoundVerifierRunner) Run(_ context.Context, spec agent.SeatSpec, _ string) agent.SeatResult {
	res := agent.SeatResult{Role: spec.Role, OK: true}
	switch spec.Role {
	case agent.RoleResearch:
		res.Findings = []string{"STABLE-FINDING"}
	case agent.RoleBuild:
		res.Proposal = largeDiff("STABLE")
	case agent.RoleVerifyCorrectness, agent.RoleVerifySecurity, agent.RoleVerifyReproduce:
		// Verifiers refute with round-varying text and all survive (accept).
		res.GoalMet = true
		res.Findings = []string{fmt.Sprintf("verifier-noise-round-%d", spec.Round)}
	case agent.RoleMeasure:
		res.Metrics = map[string]float64{"progress": float64(spec.Round) * 0.01}
	}
	return res
}

// TestConverge_VerifierNoiseDoesNotBlockConvergence guards the convergence-signal
// scope: even though verifiers and measure emit new text every round, the run
// still goes dry on the stable research finding + build proposal and converges.
func TestConverge_VerifierNoiseDoesNotBlockConvergence(t *testing.T) {
	t.Parallel()
	st, err := Run(context.Background(), Options{
		Goal: "verifier noise must not block dryness", Rounds: 0, MaxRounds: 10, DryStreakLimit: 2,
		Runner: perRoundVerifierRunner{}, Briefer: staticBriefer{}, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.Round != 3 {
		t.Errorf("stable content should converge after 3 rounds despite verifier noise; got %d", st.Round)
	}
}

// TestConverge_AllFailedRoundIsNotDry: a round where EVERY content seat failed or
// degraded must NOT read as "converged — no new progress". It counts as a failed
// round, and consecutive failed rounds stop the run with a DISTINCT reason.
func TestConverge_AllFailedRoundIsNotDry(t *testing.T) {
	t.Parallel()
	// Default OK=false with a (non-egress) warning: every seat degrades every round.
	failAll := &agent.FakeRunner{
		Default: agent.SeatResult{OK: false, Warnings: []string{"worker degraded"}},
	}
	var out bytes.Buffer
	st, err := Run(context.Background(), Options{
		Goal: "every content seat fails", Rounds: 0, MaxRounds: 10, DryStreakLimit: 2,
		Runner: failAll, Briefer: staticBriefer{}, Store: newTestStore(t), Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Two consecutive all-failed rounds trip the failed-streak stop (not the cap).
	if st.Round != 2 {
		t.Errorf("expected to stop after 2 failed rounds, got %d", st.Round)
	}
	if st.GoalMet {
		t.Errorf("a failed run must not report goal met")
	}
	if strings.Contains(out.String(), "converged") {
		t.Errorf("an all-failed run must NOT report convergence; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no content seat succeeded") {
		t.Errorf("an all-failed run must report the failed-round reason; output:\n%s", out.String())
	}
}

// TestConverge_DroppedProposalKeyPersistsAcrossResume: a verifier-refuted proposal
// has its text cleared but its stable dedupe key retained, so seenFromState (the
// resume rebuild) still knows it — it can never resurface after a process restart.
func TestConverge_DroppedProposalKeyPersistsAcrossResume(t *testing.T) {
	t.Parallel()
	prop := largeDiff("REFUTED")
	key := proposalKey(prop)

	// A resumed state whose build proposal was DROPPED but keyed.
	resumed := &state.State{Rounds: []state.RoundState{
		{Round: 1, Seats: []state.SeatOutcome{
			{Role: agent.RoleResearch, OK: true, Findings: []string{"F"}},
			{Role: agent.RoleBuild, OK: true, Proposal: "", ProposalKey: key},
		}},
	}}
	seen := seenFromState(resumed)
	if got := seen.addNew([]string{key}); got != 0 {
		t.Errorf("a dropped-but-keyed proposal must already be seen on resume; addNew reported %d new", got)
	}

	// Control: without the persisted key, the dropped proposal is invisible to a
	// resume and WOULD resurface (this is exactly the bug the key fixes).
	unkeyed := &state.State{Rounds: []state.RoundState{
		{Round: 1, Seats: []state.SeatOutcome{
			{Role: agent.RoleBuild, OK: true, Proposal: "", ProposalKey: ""},
		}},
	}}
	if got := seenFromState(unkeyed).addNew([]string{key}); got != 1 {
		t.Errorf("without a persisted key the dropped proposal is unseen on resume; addNew reported %d", got)
	}
}

// TestConverge_SeenRebuiltFromResumedState: seenFromState repopulates the dedupe
// set from prior rounds so a resumed run does not treat old items as new.
func TestConverge_SeenRebuiltFromResumedState(t *testing.T) {
	t.Parallel()
	st := &state.State{Rounds: []state.RoundState{
		{Round: 1, Seats: []state.SeatOutcome{{Role: "research", Findings: []string{"OLD"}}}},
	}}
	seen := seenFromState(st)
	if got := seen.addNew([]string{findingKey("OLD")}); got != 0 {
		t.Errorf("a finding from a prior round must already be seen; addNew reported %d new", got)
	}
	if got := seen.addNew([]string{findingKey("BRAND NEW")}); got != 1 {
		t.Errorf("a genuinely new finding should count as new; got %d", got)
	}
}
