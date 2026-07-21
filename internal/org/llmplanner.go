package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// LLMPlanner is the self-planning control plane. Each round it spawns a single
// NON-mutating CONTROL seat (deep tier, plan mode) whose brief is the goal, the
// refined goal + subgoals so far, and the accumulated run state. The control seat
// returns a strict JSON plan; the planner SANITIZES it — role and model
// allowlists, a seats-per-round cap, a bounded per-seat focus, and the
// mutating-privilege lock — and returns the round's worker roster. It re-plans
// every round from the prior round's outcomes, so the graph rewrites itself as
// work happens ("prompting each other and planning each other").
//
// It degrades gracefully: if the control seat fails, emits invalid JSON, or
// yields no valid seat, the planner falls back to the FixedPlanner roster (with a
// warning) — it never hangs and never runs an empty round.
//
// SECURITY: the planner can never escalate privilege. It reads only role/model/
// effort/focus from the plan; Mutating is derived SOLELY from the human
// --allow-mutating-build flag AND role==build (see lockMutating), never from plan
// input; focus is prompt-only text that lands only in the brief body.
type LLMPlanner struct {
	// Base carries the model/effort/repo/allow-mutating config, provides the fixed
	// fallback roster, and supplies the verifier specs for the verifier-on-edge.
	Base FixedPlanner
	// Runner runs the control seat. It is the same Runner the loop uses for workers,
	// so the control seat honors the no-egress gate and per-worker reaping. The
	// control seat is ALWAYS invoked non-mutating and plan-mode. Tests inject a fake.
	Runner agent.Runner
	// Briefer assembles the control seat's brief from control.md.
	Briefer Briefer
	// MaxSeats caps worker seats per round (0 → maxControlSeatsPerRound).
	MaxSeats int
	// Warn receives graceful-degrade notices (nil → discarded).
	Warn io.Writer
}

const (
	// maxControlSeatsPerRound caps how many worker seats the control plane may
	// schedule in one round — a hard ceiling against a runaway or adversarial plan
	// (rounds themselves stay bounded by the loop's max-rounds).
	maxControlSeatsPerRound = 6
	// maxFocusBytes bounds per-seat focus text so a huge focus can neither blow the
	// brief budget nor dominate a worker's prompt.
	maxFocusBytes = 2000
	// maxRefinedGoalBytes / maxSubgoals / maxSubgoalBytes bound the refined goal and
	// subgoals the control plane can persist into state.
	maxRefinedGoalBytes = 4000
	maxSubgoals         = 12
	maxSubgoalBytes     = 400
)

// allowedPlanRoles is the STRICT allowlist of roles the control plane may
// schedule. A role outside this set is DROPPED (never scheduled), so a plan can
// never invent a privileged, unknown, or verifier/control seat. It is a subset of
// the agent.RoleX ids on purpose.
var allowedPlanRoles = map[string]bool{
	agent.RoleResearch:   true,
	agent.RoleBuild:      true,
	agent.RoleCritic:     true,
	agent.RoleMeasure:    true,
	agent.RoleSynthesize: true,
}

// allowedPlanModels is the STRICT model allowlist. A model outside this set is
// replaced by the per-tier default, so a plan can never select an arbitrary model.
var allowedPlanModels = map[string]bool{
	agent.ModelCheap: true,
	agent.ModelDeep:  true,
}

// allowedPlanEfforts is the STRICT effort allowlist; anything else falls back to
// the run's --effort. (Effort is passed as a single argv element, so it cannot
// inject a flag even without the allowlist — this is defense in depth.)
var allowedPlanEfforts = map[string]bool{"low": true, "medium": true, "high": true}

// controlPlan is the schema the control seat emits (additionalProperties false is
// requested of the model; unknown fields are inert here because only these fields
// are ever read — an injected extra field can do nothing).
type controlPlan struct {
	RefinedGoal string        `json:"refined_goal"`
	Subgoals    []string      `json:"subgoals"`
	Seats       []controlSeat `json:"seats"`
	Stop        bool          `json:"stop"`
	Reason      string        `json:"reason"`
}

type controlSeat struct {
	Role   string `json:"role"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Focus  string `json:"focus"`
}

// verifierSpecs delegates to Base so the verifier-on-edge keeps working under the
// LLM planner (verify.go type-asserts the planner for this method).
func (p LLMPlanner) verifierSpecs() []agent.SeatSpec { return p.Base.verifierSpecs() }

// Plan implements Planner. It runs the control seat, sanitizes the plan, refines
// the goal in state, and returns the round's roster — or the fixed roster on any
// failure. It never returns an empty roster.
func (p LLMPlanner) Plan(ctx context.Context, goal string, st *state.State) []agent.SeatSpec {
	round := 1
	if st != nil {
		round = st.Round + 1
	}

	plan, err := p.planViaControl(ctx, goal, st, round)
	if err != nil {
		return p.fallback(ctx, goal, st, fmt.Sprintf("control plane unavailable (%v)", err))
	}

	seats := p.sanitize(plan)
	if len(seats) == 0 {
		return p.fallback(ctx, goal, st, "control plane returned no schedulable seat")
	}

	// Commit the accepted plan's goal refinement into state so THIS round's workers
	// and the NEXT round's control seat both see it (persisted via Merge/Save).
	applyRefinement(st, plan)

	if plan.Stop {
		p.warnf("control plane recommends stopping after this round: %s", oneLineControl(plan.Reason))
	}
	return seats
}

// planViaControl builds the control seat's brief, runs it non-mutating in plan
// mode, and parses its plan. Any failure returns an error so Plan falls back.
func (p LLMPlanner) planViaControl(ctx context.Context, goal string, st *state.State, round int) (controlPlan, error) {
	if p.Runner == nil || p.Briefer == nil {
		return controlPlan{}, errors.New("no control runner/briefer configured")
	}
	control := agent.SeatSpec{
		Role:      agent.RoleControl,
		BriefOnly: true,            // plans from state; no repo exploration needed
		McpBrain:  false,           // brain off → cheaper, smaller egress surface
		Mutating:  false,           // the control plane is ALWAYS non-mutating
		Model:     agent.ModelDeep, // deep tier (sonnet)
		Effort:    sanitizeEffort("", p.Base.Effort),
		RepoRoot:  p.Base.RepoRoot,
		Round:     round, // round-scope the control seat so it never replays a prior plan
	}
	// Belt-and-suspenders: the control seat can never be mutating.
	control.Mutating = false

	brief, _ := p.Briefer.Brief(ctx, goal, st, control, nil)
	res := p.Runner.Run(ctx, control, brief)
	if !res.OK {
		return controlPlan{}, fmt.Errorf("control seat degraded: %s", firstControlWarn(res.Warnings))
	}
	return parseControlPlan(res.Raw)
}

// parseControlPlan extracts and decodes the control seat's plan object from its
// raw output. Unknown top-level fields are tolerated (inert): only allowlisted
// fields are ever read, and every value is sanitized before use.
func parseControlPlan(raw string) (controlPlan, error) {
	obj, ok := agent.ExtractInnerJSON(raw)
	if !ok {
		return controlPlan{}, errors.New("control seat emitted no JSON plan object")
	}
	var plan controlPlan
	if err := json.Unmarshal([]byte(obj), &plan); err != nil {
		return controlPlan{}, fmt.Errorf("control plan not valid JSON: %w", err)
	}
	return plan, nil
}

// sanitize turns a raw plan into a safe roster. It drops unknown/privileged roles,
// defaults disallowed models per tier, clamps effort, bounds focus, dedupes by
// role, caps the seat count, and applies the mutating-privilege lock. The result
// is safe to run as-is; an empty result signals the caller to fall back.
func (p LLMPlanner) sanitize(plan controlPlan) []agent.SeatSpec {
	limit := p.MaxSeats
	if limit <= 0 {
		limit = maxControlSeatsPerRound
	}

	var out []agent.SeatSpec
	for _, cs := range plan.Seats {
		role := strings.ToLower(strings.TrimSpace(cs.Role))
		if !allowedPlanRoles[role] {
			continue // drop unknown / privileged / verifier / control roles
		}
		mcpBrain, briefOnly := planTier(role)
		out = append(out, agent.SeatSpec{
			Role:      role,
			McpBrain:  mcpBrain,
			BriefOnly: briefOnly,
			Model:     sanitizeModel(cs.Model, mcpBrain),
			Effort:    sanitizeEffort(cs.Effort, p.Base.Effort),
			RepoRoot:  p.Base.RepoRoot,
			Focus:     boundFocus(cs.Focus),
			// Mutating deliberately NOT set here — lockMutating derives it below.
		})
	}
	out = dedupeByRole(out)
	if len(out) > limit {
		out = out[:limit]
	}
	// The single mutating-privilege choke point: derive Mutating from (human flag,
	// role), never from the plan. No plan can escalate a seat past this line.
	return lockMutating(out, p.Base.AllowMutating)
}

// fallback returns the fixed roster with a degrade warning. It is the safe landing
// whenever the control plane cannot produce a usable plan.
func (p LLMPlanner) fallback(ctx context.Context, goal string, st *state.State, why string) []agent.SeatSpec {
	p.warnf("control plane: %s; falling back to the fixed plan for this round", why)
	return p.Base.Plan(ctx, goal, st)
}

func (p LLMPlanner) warnf(format string, args ...any) {
	if p.Warn == nil {
		return
	}
	fmt.Fprintf(p.Warn, "entire-loop: "+format+"\n", args...)
}

// lockMutating is the mutating-privilege lock: it (re)derives every seat's
// Mutating bit SOLELY from the human allowMutating flag AND role==build, and
// clears it everywhere else. No plan — however adversarial — can turn a seat into
// a mutating/bypass worker, because Mutating is a pure function of (human flag,
// role) and never of planner output. Every LLM-planned roster passes through here.
func lockMutating(seats []agent.SeatSpec, allowMutating bool) []agent.SeatSpec {
	for i := range seats {
		seats[i].Mutating = allowMutating && seats[i].Role == agent.RoleBuild
	}
	return seats
}

// planTier returns the hybrid brain wiring for a plannable role, mirroring
// FixedPlanner: research/critic are DEEP (brain on, not brief-only); the rest are
// cheap brief-only seats.
func planTier(role string) (mcpBrain, briefOnly bool) {
	switch role {
	case agent.RoleResearch, agent.RoleCritic:
		return true, false
	default:
		return false, true
	}
}

// sanitizeModel returns an allowlisted model or the per-tier default.
func sanitizeModel(model string, deepTier bool) string {
	if m := strings.TrimSpace(model); allowedPlanModels[m] {
		return m
	}
	if deepTier {
		return agent.ModelDeep
	}
	return agent.ModelCheap
}

// sanitizeEffort returns an allowlisted effort or the given fallback (the run's
// --effort, which may be empty).
func sanitizeEffort(effort, fallback string) string {
	if e := strings.ToLower(strings.TrimSpace(effort)); allowedPlanEfforts[e] {
		return e
	}
	return fallback
}

// boundFocus trims and length-bounds focus text (rune-safe). Focus is opaque
// prompt text; it needs no flag/shell escaping because it only ever lands in the
// brief body via the ${FOCUS} marker.
func boundFocus(focus string) string {
	f := strings.TrimSpace(focus)
	if len(f) <= maxFocusBytes {
		return f
	}
	n := maxFocusBytes
	for n > 0 && !utf8.RuneStart(f[n]) {
		n--
	}
	return f[:n]
}

// applyRefinement commits an accepted plan's goal refinement into state (bounded).
func applyRefinement(st *state.State, plan controlPlan) {
	if st == nil {
		return
	}
	if rg := strings.TrimSpace(plan.RefinedGoal); rg != "" {
		st.RefinedGoal = boundText(rg, maxRefinedGoalBytes)
	}
	if subs := cleanSubgoals(plan.Subgoals); len(subs) > 0 {
		st.Subgoals = subs
	}
}

// cleanSubgoals trims, drops empties, bounds each, and caps the count.
func cleanSubgoals(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, boundText(s, maxSubgoalBytes))
		if len(out) >= maxSubgoals {
			break
		}
	}
	return out
}

// boundText rune-safely truncates s to at most max bytes.
func boundText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	n := max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func firstControlWarn(warnings []string) string {
	if len(warnings) == 0 {
		return "no output"
	}
	return oneLineControl(warnings[0])
}

func oneLineControl(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
