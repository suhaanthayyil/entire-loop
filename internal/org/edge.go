package org

import (
	"context"
	"fmt"
	"strings"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// An EDGE is a first-class transition in the round graph: a function/predicate
// over the typed round state that decides the next node(s)/seat(s) or a gate
// outcome. Nodes are agent seats (each its own worker loop); edges are the typed
// transitions between them. Formalizing them gives the loop state-machine rigor —
// nodes + typed transitions — and makes each transition unit-testable in
// isolation.
//
// The taxonomy is a small, closed set (do NOT grow it without a reason):
//
//   - DataEdge        output→input: projects an upstream seat's validated outcome
//     into a downstream seat's brief (the pipeline edges).
//   - ConditionalEdge router: a deterministic predicate over a validated signal
//     (the build proposal's diff size) that selects the verify path.
//   - VerifierEdge    adversarial gate: N skeptics must fail to refute an item
//     before it is accepted into the result.
//   - MeasureEdge     external signal: runs a user-configured command and parses
//     its JSON stdout into typed round metrics (measure.go).
//   - CycleEdge       loop-until-dry: folds a round outcome into a continue/stop
//     decision (goal-met / dry-streak / fail-streak / cap).
//
// The Edge interface stays deliberately minimal — Name + Kind, for enumeration
// and introspection. Each concrete edge adds ONE focused evaluation method
// (Project / Route / Gate / Measure / decide) on its own type; the interface is
// not widened to force every edge through one bloated signature.
type Edge interface {
	Name() string
	Kind() EdgeKind
}

// EdgeKind labels the taxonomy slot an edge belongs to.
type EdgeKind int

const (
	KindData EdgeKind = iota
	KindConditional
	KindVerifier
	KindMeasure
	KindCycle
)

func (k EdgeKind) String() string {
	switch k {
	case KindData:
		return "data"
	case KindConditional:
		return "conditional"
	case KindVerifier:
		return "verifier"
	case KindMeasure:
		return "measure"
	case KindCycle:
		return "cycle"
	default:
		return "unknown"
	}
}

// AllEdgeKinds is the closed taxonomy, in canonical order.
var AllEdgeKinds = []EdgeKind{KindData, KindConditional, KindVerifier, KindMeasure, KindCycle}

// Taxonomy returns one representative of every edge kind wired into the loop. It
// exists so the edge set is enumerable/introspectable (and asserted complete in a
// test) — it is not a runtime dispatch table.
func Taxonomy() []Edge {
	return []Edge{
		DataEdge{},
		ConditionalEdge{},
		VerifierEdge{},
		MeasureEdge{},
		&CycleEdge{},
	}
}

// RoundView is the typed, read-only state an edge evaluates: the seat outcomes
// produced so far THIS round plus the accumulated run state. Edges are pure
// functions over this view (MeasureEdge is the sole, explicit exception — it is
// the external-signal edge). It is the state-machine's current-node context.
type RoundView struct {
	Round   int
	Results []agent.SeatResult
	State   *state.State
}

// BuildOutcome returns the most recent build seat result in the round, if any.
func (rv RoundView) BuildOutcome() (agent.SeatResult, bool) {
	return lastByRole(rv.Results, agent.RoleBuild)
}

// ---- DataEdge: output → input -------------------------------------------------

// DataEdge carries the validated output of one or more upstream seats into a
// downstream seat's brief. From lists the upstream roles, in priority order.
type DataEdge struct{ From []string }

func (e DataEdge) Kind() EdgeKind { return KindData }

func (e DataEdge) Name() string {
	if len(e.From) == 0 {
		return "data:(fan-in)"
	}
	return "data:" + strings.Join(e.From, "+")
}

// Project pulls the outcomes of this edge's upstream roles out of the results
// produced so far, in results order, projected to persisted SeatOutcomes — the
// DATA that flows across the edge into the next node's brief.
func (e DataEdge) Project(results []agent.SeatResult) []state.SeatOutcome {
	var out []state.SeatOutcome
	for _, r := range results {
		for _, role := range e.From {
			if r.Role == role {
				out = append(out, toOutcome(r))
			}
		}
	}
	return out
}

// ---- ConditionalEdge: the router ---------------------------------------------

// ConditionalEdge is the router as a first-class edge: a deterministic predicate
// over the typed round state (the build proposal's diff size) that selects how
// hard the round verifies the change. It is the single Edge implementation of the
// routing decision — the raw analyzeDiff/routeForProposal helpers remain as its
// pure, unit-tested internals.
type ConditionalEdge struct{}

func (ConditionalEdge) Name() string   { return "route-by-proposal-size" }
func (ConditionalEdge) Kind() EdgeKind { return KindConditional }

// Route inspects the round's build outcome and returns the verify route. No build
// outcome (or an empty proposal) is trivially small → solo-critic.
func (ConditionalEdge) Route(rv RoundView) route {
	b, ok := rv.BuildOutcome()
	if !ok {
		return routeSoloCritic
	}
	return routeForProposal(b.Proposal)
}

// ---- VerifierEdge: the adversarial gate --------------------------------------

// VerifierEdge is the verifier-on-edge as a first-class edge: before a large
// proposal is accepted into the result, N diverse skeptics each try to REFUTE it
// and it survives only on a majority. The gate mechanism (fan-out + majority) is
// Options.verifyOnEdge; this type names the edge and delegates, so the taxonomy
// stays honest without duplicating the mechanism.
type VerifierEdge struct{}

func (VerifierEdge) Name() string   { return "verifier-on-edge" }
func (VerifierEdge) Kind() EdgeKind { return KindVerifier }

// Gate runs the adversarial verifier audit over the item and returns its result.
func (VerifierEdge) Gate(ctx context.Context, opts Options, round int, st *state.State, item []state.SeatOutcome) verifierResult {
	return opts.verifyOnEdge(ctx, round, st, item)
}

// ---- CycleEdge: loop-until-dry -----------------------------------------------

// cycleAction is the CycleEdge's typed transition out of a round.
type cycleAction int

const (
	cycleContinue    cycleAction = iota // run the next round
	cycleStop                           // stop the loop (dry/fail streak or cap-adjacent)
	cycleStopGoalMet                    // stop immediately: the goal was met this round
)

// cycleDecision is one CycleEdge transition: an action plus the human-readable log
// line to emit for it (empty when there is nothing to say).
type cycleDecision struct {
	action  cycleAction
	message string
}

// CycleEdge is the loop-until-dry transition as a first-class, stateful edge. It
// owns the convergence bookkeeping across rounds — the set of every
// finding/proposal key ever seen and the dry/fail streaks — and folds each round's
// outcome into a typed continue/stop decision. Extracting it makes the convergence
// state machine directly unit-testable (feed it round outcomes, assert transitions)
// instead of buried in the run loop.
//
// What actually converges: by default a run stops on the round VERDICT (goal-met)
// or a DRY STREAK (no new finding/proposal keys), and a FAIL STREAK / round cap
// bound it. An external measurement stops the loop ONLY when the run configures a
// metric threshold (Options.ConvergeMetric/ConvergeAt): crossing it sets the round's
// goal-met in runRound, which this edge then reads as the immediate goal-met stop.
// Absent that config, the MeasureEdge's metrics feed the reorg edge but do NOT drive
// convergence.
type CycleEdge struct {
	seen       seenSet
	dryStreak  int
	failStreak int
	dryLimit   int
	fixedCount bool
}

func (*CycleEdge) Name() string   { return "loop-until-dry" }
func (*CycleEdge) Kind() EdgeKind { return KindCycle }

// newCycleEdge builds the convergence edge, rebuilding the seen set from any
// resumed state so a resume does not treat old items as new.
func newCycleEdge(st *state.State, dryLimit int, fixedCount bool) *CycleEdge {
	return &CycleEdge{
		seen:       seenFromState(st),
		dryLimit:   dryLimit,
		fixedCount: fixedCount,
	}
}

// decide folds one completed round into the next transition. It reproduces the
// loop-until-dry rules exactly:
//   - goal met → stop immediately;
//   - fixed-count mode → always continue (the round cap bounds it);
//   - a round where no content seat succeeded → a FAILED round (distinct from
//     dry): it resets the dry streak, advances the fail streak, and stops with a
//     distinct reason once the streak hits the limit — its keys are NOT folded
//     into seen;
//   - otherwise fold the round's keys into seen: zero new keys advances the dry
//     streak; the streak hitting the limit converges the run.
func (c *CycleEdge) decide(round int, rs state.RoundState, rawKeys []string) cycleDecision {
	if rs.GoalMet {
		return cycleDecision{cycleStopGoalMet, fmt.Sprintf("loop: goal met after round %d — stopping\n", round)}
	}
	if c.fixedCount {
		return cycleDecision{cycleContinue, ""}
	}

	if !roundHadOKProgress(rs.Seats) {
		c.dryStreak = 0
		c.failStreak++
		if c.failStreak >= c.dryLimit {
			return cycleDecision{cycleStop, fmt.Sprintf(
				"loop: stopping after %d consecutive failed round(s) — no content seat succeeded (round %d)\n", c.failStreak, round)}
		}
		return cycleDecision{cycleContinue, ""}
	}
	c.failStreak = 0

	if c.seen.addNew(rawKeys) == 0 {
		c.dryStreak++
	} else {
		c.dryStreak = 0
	}
	if c.dryStreak >= c.dryLimit {
		return cycleDecision{cycleStop, fmt.Sprintf(
			"loop: converged — %d consecutive dry round(s) with no new progress; stopping after round %d\n", c.dryStreak, round)}
	}
	return cycleDecision{cycleContinue, ""}
}

// ---- plan-mutability axis -----------------------------------------------------

// PlanMode is the plan-mutability axis, orthogonal to the --planner choice
// (llm|fixed). It decides WHEN the seat/node DAG is planned, not HOW.
type PlanMode int

const (
	// PlanModeDynamic re-plans the DAG every round from the accumulated state
	// (the graph rewrites itself). This is the default and the current behavior.
	PlanModeDynamic PlanMode = iota
	// PlanModeImmutable plans the whole DAG ONCE up front and executes it every
	// round with bounded per-node recovery (retry then degrade) and NO mid-run
	// re-plan or runtime reorg — the fixed-script position.
	PlanModeImmutable
)

func (m PlanMode) String() string {
	if m == PlanModeImmutable {
		return "immutable"
	}
	return "dynamic"
}

// defaultNodeRetries is the bounded per-node recovery budget applied in immutable
// plan-mode when the run does not set one: one retry (two attempts) per failed
// node before it is left degraded. Dynamic mode uses 0 — there, re-planning is the
// recovery, so a failed node is simply re-planned next round.
const defaultNodeRetries = 1

// cloneSeats returns an independent copy of a seat roster so freezing it (immutable
// plan-mode) is never disturbed by a later in-place round stamp.
func cloneSeats(seats []agent.SeatSpec) []agent.SeatSpec {
	return append([]agent.SeatSpec(nil), seats...)
}
