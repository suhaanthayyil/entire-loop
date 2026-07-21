// Package templates embeds the per-seat prompt templates shipped with the loop
// plugin and exposes them by role. A template is a role's task prompt: it says
// what to read (graph/brain), what to produce, and the exact JSON schema to
// return. The brief builder substitutes the ${GOAL}/${STATE}/${GRAPH}/${BRIEF}/
// ${METRICS} markers before the text is handed to a worker on stdin.
package templates

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed templates/*.md
var templatesFS embed.FS

// Markers recognized in a seat template. They are substituted by the brief
// builder; any left unfilled are cleared so a stray marker never reaches a
// worker.
//
// ${UPSTREAM} carries the validated output of a seat's upstream nodes in the
// round DAG (research → build → critic → …): it is how DATA flows across an edge,
// so a downstream seat consumes its upstream's real findings/proposal rather than
// a shared window. ${LENS} is the refutation angle handed to a skeptic verifier.
//
// ${REFINED_GOAL} and ${SUBGOALS} carry the LLM control plane's evolving goal
// restatement (from state) so every worker plans against the sharpened goal.
// ${FOCUS} carries the per-seat, per-round focus text the control plane composed
// for THIS seat ("prompting each other") — it lands only in the brief body.
const (
	MarkerGoal        = "${GOAL}"
	MarkerState       = "${STATE}"
	MarkerGraph       = "${GRAPH}"
	MarkerBrief       = "${BRIEF}"
	MarkerMetrics     = "${METRICS}"
	MarkerUpstream    = "${UPSTREAM}"
	MarkerLens        = "${LENS}"
	MarkerRefinedGoal = "${REFINED_GOAL}"
	MarkerSubgoals    = "${SUBGOALS}"
	MarkerFocus       = "${FOCUS}"
)

// AllMarkers is the full set, used to blank any unfilled markers.
var AllMarkers = []string{
	MarkerGoal, MarkerState, MarkerGraph, MarkerBrief, MarkerMetrics,
	MarkerUpstream, MarkerLens, MarkerRefinedGoal, MarkerSubgoals, MarkerFocus,
}

// Load returns the raw template text for a role (e.g. "research"). YAML
// frontmatter, if present, is stripped so a leading "---" never reaches an
// agent that treats a positional string as flags. Skeptic verifier roles
// (verify-correctness/security/reproduce) all share the single verify.md
// template; their lens rides in via ${LENS}.
func Load(role string) (string, error) {
	name := "templates/" + templateFile(role) + ".md"
	data, err := templatesFS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read seat template %q: %w", role, err)
	}
	return stripFrontmatter(string(data)), nil
}

// templateFile maps a seat role to its template basename. All verify-* skeptic
// roles collapse onto the shared "verify" template.
func templateFile(role string) string {
	if strings.HasPrefix(role, "verify-") {
		return "verify"
	}
	return role
}

// Render substitutes the provided marker→value pairs into the role template and
// blanks any markers left unfilled.
func Render(role string, values map[string]string) (string, error) {
	text, err := Load(role)
	if err != nil {
		return "", err
	}
	return substitute(text, values), nil
}

// substitute does a single left-to-right replacement pass over a FIXED marker
// order (AllMarkers), replacing each marker with its value (a marker absent from
// values blanks to ""). A single pass is both deterministic — independent of map
// iteration order — and injection-safe: strings.NewReplacer never re-scans text
// it just substituted, so a value that itself contains a marker literal (e.g. a
// graph section holding the string "${METRICS}") is left intact instead of being
// re-expanded or blanked.
func substitute(text string, values map[string]string) string {
	pairs := make([]string, 0, len(AllMarkers)*2)
	for _, marker := range AllMarkers {
		pairs = append(pairs, marker, values[marker])
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return content
	}
	lines := strings.Split(content, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\r\n")
		}
	}
	return content
}
