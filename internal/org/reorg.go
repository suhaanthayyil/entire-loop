package org

import (
	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// Reorg is the runtime-reorganization seam. Before a round fans out, the loop
// gives the planned seats to Reorg.Apply, which may adjust the roster based on
// how prior rounds went. The MVP wires in NoopReorg so the seam exists without
// any behavior; Phase B implements the rules below.
//
// Phase-B reorg rules (NOT implemented in the MVP — documented here as the
// contract the seam is designed for):
//   - small→solo: a small, well-scoped goal collapses to a single soloist seat
//     instead of the full research/build/critic/measure roster.
//   - fail-cluster→+critic: a round whose critic reports goalMet=false with
//     correctness gaps adds an extra critic (or a specialized reviewer) next round.
//   - budget>progress→collapse: when spend outpaces measured progress, collapse
//     the roster toward the cheapest seats that can still advance the goal.
//   - 2×fix→promote: a seat that has fixed the same defect twice is promoted
//     (deeper wiring / higher effort) so the recurring problem is addressed at root.
type Reorg interface {
	Apply(seats []agent.SeatSpec, st *state.State) []agent.SeatSpec
}

// NoopReorg is the MVP default: it returns the planned seats unchanged. It exists
// so the loop always calls through the reorg seam, keeping the extension point
// live even before any rule is implemented.
type NoopReorg struct{}

// Apply implements Reorg by returning the seats unchanged.
func (NoopReorg) Apply(seats []agent.SeatSpec, _ *state.State) []agent.SeatSpec {
	return seats
}
