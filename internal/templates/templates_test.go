package templates

import "testing"

// TestSubstitute_Deterministic verifies the substitution is a single, fixed-order
// pass: it is independent of map iteration order AND a value that itself contains
// a marker literal is never re-expanded (the old map-loop + blank approach would
// either blank or re-expand it depending on map order).
func TestSubstitute_Deterministic(t *testing.T) {
	t.Parallel()
	text := "goal=" + MarkerGoal + " graph=" + MarkerGraph + " metrics=" + MarkerMetrics + " state=" + MarkerState
	values := map[string]string{
		MarkerGoal: "G",
		// GRAPH's value literally contains the ${METRICS} marker: it must survive
		// verbatim, neither blanked nor expanded to "M".
		MarkerGraph:   MarkerMetrics,
		MarkerMetrics: "M",
		// MarkerState is intentionally omitted → must blank to "".
	}

	want := "goal=G graph=" + MarkerMetrics + " metrics=M state="
	first := substitute(text, values)
	if first != want {
		t.Fatalf("substitute = %q, want %q", first, want)
	}

	// Stable across many repeated calls regardless of map iteration order.
	for i := 0; i < 100; i++ {
		if got := substitute(text, values); got != first {
			t.Fatalf("nondeterministic output on iteration %d: %q != %q", i, got, first)
		}
	}
}

// TestSubstitute_BlanksUnfilled confirms every known marker with no value blanks.
func TestSubstitute_BlanksUnfilled(t *testing.T) {
	t.Parallel()
	text := MarkerGoal + MarkerState + MarkerGraph + MarkerBrief + MarkerMetrics
	if got := substitute(text, map[string]string{MarkerGoal: "X"}); got != "X" {
		t.Errorf("substitute = %q, want %q", got, "X")
	}
}
