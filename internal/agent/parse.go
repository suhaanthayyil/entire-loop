package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SeatResult is the parsed outcome of running one worker seat. A result is never
// a hard failure: a garbled or missing envelope degrades to OK=false plus a
// warning so the round continues (graceful degradation), never an abort.
type SeatResult struct {
	Role     string
	OK       bool
	Warnings []string
	CostUSD  float64
	NumTurns int
	Findings []string
	Proposal string
	Metrics  map[string]float64
	Verdict  string
	GoalMet  bool
	Raw      string
}

// claudeEnvelope is the subset of the `claude --output-format json` envelope the
// loop reads. `result` carries the model's final text, which itself contains the
// seat's JSON object.
type claudeEnvelope struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
}

// seatRaw is the on-the-wire shape a seat is asked to emit. Every field is
// optional; a seat emits the subset its role produces. Metrics is captured raw
// so a non-numeric value degrades that one key rather than the whole object.
type seatRaw struct {
	Findings []string           `json:"findings"`
	Proposal string             `json:"proposal"`
	Metrics  map[string]float64 `json:"metrics"`
	Verdict  string             `json:"verdict"`
	GoalMet  *bool              `json:"goalMet"`
	Score    *float64           `json:"score"`
}

// ParseSeatOutput turns a worker's raw stdout into a SeatResult. It first reads
// the claude JSON envelope (for cost/turns/error), then leniently parses the
// inner result text for the seat's JSON object. Any failure becomes a warning on
// the result — it is never returned as an error.
func ParseSeatOutput(role, stdout string) SeatResult {
	result := SeatResult{Role: role, Raw: stdout}

	inner := stdout
	if env, ok := parseEnvelope(stdout); ok {
		result.CostUSD = env.TotalCostUSD
		result.NumTurns = env.NumTurns
		if env.IsError {
			result.Warnings = append(result.Warnings, "worker envelope reported is_error=true")
		}
		if strings.TrimSpace(env.Result) != "" {
			inner = env.Result
		}
	} else {
		result.Warnings = append(result.Warnings, "worker output was not a claude JSON envelope; parsing raw output")
	}

	// Balanced-object extraction FIRST, on the raw text: it tracks string literals,
	// so a valid object whose values contain literal ``` fences (or stray braces)
	// is preserved. Only if the raw text yields no balanced object do we strip a
	// code fence and retry — stripping first would slice through a value that
	// legitimately contains ``` and destroy an otherwise-valid object.
	jsonText, ok := extractOuterJSONObject(inner)
	if !ok {
		jsonText, ok = extractOuterJSONObject(stripJSONFences(inner))
	}
	if !ok {
		result.Warnings = append(result.Warnings, "seat output contained no JSON object")
		return result
	}
	var raw seatRaw
	if err := json.Unmarshal([]byte(jsonText), &raw); err != nil {
		// A non-numeric metric (or other soft error) should not lose the rest of
		// the object: retry with a lenient shape that drops the metrics field.
		if lenient, ok2 := parseLenient(jsonText); ok2 {
			raw = lenient
		} else {
			result.Warnings = append(result.Warnings, fmt.Sprintf("seat output not valid JSON: %v", err))
			return result
		}
	}

	result.Findings = dedupeStrings(raw.Findings)
	result.Proposal = raw.Proposal
	result.Metrics = raw.Metrics
	result.Verdict = strings.TrimSpace(raw.Verdict)
	if raw.GoalMet != nil {
		result.GoalMet = *raw.GoalMet
	}
	// A seat that produced a parseable JSON object and no envelope error counts as
	// OK, even if its particular fields are sparse.
	result.OK = !hasWarning(result.Warnings, "is_error")
	return result
}

// parseLenient re-parses an object that failed strict decode by first stripping
// the metrics field to a raw map and coercing only its numeric entries. It
// returns ok=false if even the reduced object cannot be decoded.
func parseLenient(jsonText string) (seatRaw, bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonText), &envelope); err != nil {
		return seatRaw{}, false
	}
	var raw seatRaw
	_ = json.Unmarshal(envelope["findings"], &raw.Findings)
	_ = json.Unmarshal(envelope["proposal"], &raw.Proposal)
	_ = json.Unmarshal(envelope["verdict"], &raw.Verdict)
	if b, ok := envelope["goalMet"]; ok {
		var v bool
		if json.Unmarshal(b, &v) == nil {
			raw.GoalMet = &v
		}
	}
	if s, ok := envelope["score"]; ok {
		var v float64
		if json.Unmarshal(s, &v) == nil {
			raw.Score = &v
		}
	}
	raw.Metrics = coerceMetrics(envelope["metrics"])
	return raw, true
}

// coerceMetrics extracts only the numeric entries from a metrics object,
// dropping any value that is not a JSON number.
func coerceMetrics(raw json.RawMessage) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	out := map[string]float64{}
	for k, v := range entries {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out[k] = f
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ExtractInnerJSON returns the first balanced JSON object embedded in a worker's
// raw stdout. It unwraps a `claude --output-format json` envelope (reading its
// `result` text) when present, then strips an optional code fence — mirroring the
// exact extraction ParseSeatOutput performs. This lets a caller read a seat's
// structured output that does NOT fit the seatRaw schema (e.g. the control seat's
// plan object) using identical, injection-aware unwrapping. ok is false when no
// balanced object is present. It never executes anything and is pure.
func ExtractInnerJSON(raw string) (string, bool) {
	inner := raw
	if env, ok := parseEnvelope(raw); ok && strings.TrimSpace(env.Result) != "" {
		inner = env.Result
	}
	if obj, ok := extractOuterJSONObject(inner); ok {
		return obj, true
	}
	return extractOuterJSONObject(stripJSONFences(inner))
}

func parseEnvelope(s string) (claudeEnvelope, bool) {
	var env claudeEnvelope
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "{") {
		return claudeEnvelope{}, false
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return claudeEnvelope{}, false
	}
	// The envelope is only meaningful if it carries the fields we key on. A bare
	// seat object (no cost/result/type) is not an envelope.
	if env.Type == "" && env.Result == "" && env.TotalCostUSD == 0 && env.NumTurns == 0 && !env.IsError {
		return claudeEnvelope{}, false
	}
	return env, true
}

// stripJSONFences removes a leading/embedded ```json fence wrapper so the object
// extractor sees bare JSON. Content without fences is returned unchanged.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "```") {
		return s
	}
	if idx := strings.Index(s, "```"); idx >= 0 {
		rest := s[idx+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			lang := strings.TrimSpace(rest[:nl])
			if lang == "json" || lang == "" || !strings.Contains(lang, "{") {
				rest = rest[nl+1:]
			}
		}
		if closeIdx := strings.Index(rest, "```"); closeIdx >= 0 {
			rest = rest[:closeIdx]
		}
		return strings.TrimSpace(rest)
	}
	return s
}

// extractOuterJSONObject returns the substring from the first '{' to its matching
// '}', tracking string literals so a brace inside a JSON string does not throw
// off the balance. ok=false when no balanced object is present.
func extractOuterJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

func dedupeStrings(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
