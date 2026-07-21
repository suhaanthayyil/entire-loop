package org

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// seenSet tracks every finding/proposal key the run has EVER surfaced — accepted
// or rejected. Loop-until-dry dedupes against this, not against confirmed items:
// a rejected proposal that reappears next round is already in `seen`, so it does
// not read as new progress and the run can converge (course step 11). If we
// deduped against accepted-only, rejected items would resurface every round and
// the loop would never go dry.
type seenSet map[string]struct{}

// seenFromState rebuilds the seen set from a resumed run's prior rounds so a
// resume does not treat old items as new.
func seenFromState(st *state.State) seenSet {
	seen := seenSet{}
	if st == nil {
		return seen
	}
	for _, rd := range st.Rounds {
		for _, k := range roundKeys(rd.Seats) {
			seen[k] = struct{}{}
		}
	}
	return seen
}

// addNew folds keys into the set and returns how many were NOT already present.
// A round that adds zero new keys is "dry".
func (s seenSet) addNew(keys []string) int {
	added := 0
	for _, k := range keys {
		if _, ok := s[k]; !ok {
			s[k] = struct{}{}
			added++
		}
	}
	return added
}

// resultKeys extracts the convergence keys a set of round results surfaced: one
// per distinct finding and one per non-empty proposal, from the CONTENT seats
// only. Verifier refutations and measure metrics are per-round evaluation noise —
// counting them as "progress" would keep the run from ever going dry — so they are
// excluded (see isProgressRole).
func resultKeys(results []agent.SeatResult) []string {
	var keys []string
	for _, r := range results {
		if !isProgressRole(r.Role) {
			continue
		}
		for _, f := range r.Findings {
			if k := findingKey(f); k != "" {
				keys = append(keys, k)
			}
		}
		if k := proposalKey(r.Proposal); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// roundKeys is resultKeys over persisted seat outcomes (for resume). It falls
// back to the persisted ProposalKey when the proposal text is absent: a
// verifier-refuted proposal has its Proposal cleared but its stable key retained,
// so a resume rebuilds `seen` correctly and the dropped proposal never resurfaces.
func roundKeys(seats []state.SeatOutcome) []string {
	var keys []string
	for _, s := range seats {
		if !isProgressRole(s.Role) {
			continue
		}
		for _, f := range s.Findings {
			if k := findingKey(f); k != "" {
				keys = append(keys, k)
			}
		}
		if k := proposalKey(s.Proposal); k != "" {
			keys = append(keys, k)
		} else if s.ProposalKey != "" {
			keys = append(keys, s.ProposalKey)
		}
	}
	return keys
}

// roundHadOKProgress reports whether at least one CONTENT (progress-role) seat in
// the round succeeded. A round where every content seat failed or degraded is not
// convergence — it is a failed round — so it must NOT advance the dry streak.
func roundHadOKProgress(seats []state.SeatOutcome) bool {
	for _, s := range seats {
		if s.OK && isProgressRole(s.Role) {
			return true
		}
	}
	return false
}

// isProgressRole reports whether a seat surfaces durable findings/proposals that
// count toward "new progress". Skeptic verifiers (verify-*) and the measure seat
// produce round-local evaluation output, not progress, so they are excluded from
// the convergence signal.
func isProgressRole(role string) bool {
	switch {
	case role == agent.RoleMeasure:
		return false
	case strings.HasPrefix(role, "verify-"):
		return false
	default:
		return true
	}
}

// findingKey normalizes a finding to a dedupe key: trimmed, lowercased, with
// internal whitespace collapsed so cosmetic reformatting does not read as new.
func findingKey(f string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(f), " "))
	if norm == "" {
		return ""
	}
	return "finding:" + norm
}

// proposalKey hashes a proposal so an identical diff across rounds collapses to
// one key. Empty proposals produce no key.
func proposalKey(p string) string {
	norm := strings.Join(strings.Fields(p), " ")
	if norm == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(norm))
	return "proposal:" + hex.EncodeToString(sum[:8])
}
