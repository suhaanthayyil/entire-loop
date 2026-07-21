package org

import (
	"context"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// verifierResult is the outcome of a verifier-on-edge audit.
type verifierResult struct {
	ran       bool               // whether an audit was performed at all
	survivors int                // verifiers that could NOT refute the item
	total     int                // verifiers spawned
	accepted  bool               // survivors >= majority
	seats     []agent.SeatResult // raw verifier seat results (for the round record)
}

// verifyOnEdge is the guard on the edge before a finding/proposal is accepted
// into the result (course step 9): it spawns N=3 independent skeptic verifiers
// with diverse lenses (correctness / security / reproduce), each prompted to
// REFUTE the item, and keeps it only if a MAJORITY (>= 2 of 3) survive the
// refutation. The verifiers fan out in parallel, bounded by the --jobs cap.
func (opts Options) verifyOnEdge(ctx context.Context, round int, st *state.State, item []state.SeatOutcome) verifierResult {
	specs := opts.verifierSpecs()
	if len(specs) == 0 {
		// No verifier planner configured: nothing to audit, accept by default.
		return verifierResult{ran: false, accepted: true}
	}
	stampRound(specs, round)

	seats := opts.fanOutSeats(ctx, specs, st, item)
	survivors := verifierSurvivors(seats)
	return verifierResult{
		ran:       true,
		survivors: survivors,
		total:     len(seats),
		accepted:  majorityAccepts(survivors, len(seats)),
		seats:     seats,
	}
}

// verifierSurvivors counts verifiers that could NOT refute the item. A verifier
// signals "the item withstood my attack" with OK && GoalMet=true; a not-OK
// (degraded) verifier does not count as a survivor — an unreliable audit must not
// wave an item through.
func verifierSurvivors(results []agent.SeatResult) int {
	n := 0
	for _, r := range results {
		if r.OK && r.GoalMet {
			n++
		}
	}
	return n
}

// majorityAccepts reports whether the survivors form a strict majority of the
// verifiers that were spawned (>= ceil((total+1)/2), i.e. 2 of 3).
func majorityAccepts(survivors, total int) bool {
	if total <= 0 {
		return true
	}
	return survivors*2 > total
}

// verifierSpecs returns the skeptic verifier seats from the planner when it
// supports them, else nil.
func (opts Options) verifierSpecs() []agent.SeatSpec {
	if vp, ok := opts.Planner.(interface{ verifierSpecs() []agent.SeatSpec }); ok {
		return vp.verifierSpecs()
	}
	return nil
}
