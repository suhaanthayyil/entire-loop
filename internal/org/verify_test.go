package org

import (
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
)

func TestVerifierSurvivorsAndMajority(t *testing.T) {
	t.Parallel()
	// A verifier "survives" (item withstood its attack) only when OK && GoalMet.
	results := []agent.SeatResult{
		{Role: agent.RoleVerifyCorrectness, OK: true, GoalMet: true}, // survives
		{Role: agent.RoleVerifySecurity, OK: true, GoalMet: true},    // survives
		{Role: agent.RoleVerifyReproduce, OK: true, GoalMet: false},  // refuted
	}
	if got := verifierSurvivors(results); got != 2 {
		t.Fatalf("survivors = %d, want 2", got)
	}
	if !majorityAccepts(2, 3) {
		t.Errorf("2/3 survivors should accept")
	}

	// A degraded (not-OK) verifier is not a survivor even if GoalMet is set.
	degraded := []agent.SeatResult{
		{OK: false, GoalMet: true},
		{OK: true, GoalMet: false},
		{OK: true, GoalMet: false},
	}
	if got := verifierSurvivors(degraded); got != 0 {
		t.Errorf("survivors = %d, want 0 (degraded verifier does not count)", got)
	}
	if majorityAccepts(1, 3) {
		t.Errorf("1/3 survivors must NOT accept")
	}
	if !majorityAccepts(0, 0) {
		t.Errorf("no verifiers spawned should accept by default")
	}
}
