package agent

import "testing"

func TestParseSeatOutput_LenientInner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		stdout       string
		wantOK       bool
		wantVerdict  string
		wantGoalMet  bool
		wantFindings int
		wantWarnSub  string // if set, at least one warning must contain it
	}{
		{
			name:         "clean envelope with inner json",
			stdout:       `{"type":"result","is_error":false,"total_cost_usd":0.0123,"num_turns":3,"result":"{\"findings\":[\"a\",\"b\"],\"verdict\":\"ok\"}"}`,
			wantOK:       true,
			wantVerdict:  "ok",
			wantFindings: 2,
		},
		{
			name:        "inner wrapped in json code fence",
			stdout:      "{\"type\":\"result\",\"total_cost_usd\":0.5,\"result\":\"```json\\n{\\\"goalMet\\\":true,\\\"verdict\\\":\\\"done\\\"}\\n```\"}",
			wantOK:      true,
			wantVerdict: "done",
			wantGoalMet: true,
		},
		{
			name:        "inner has trailing prose after the object",
			stdout:      `{"type":"result","total_cost_usd":0.1,"result":"{\"verdict\":\"fine\"} and that is my final answer, thanks!"}`,
			wantOK:      true,
			wantVerdict: "fine",
		},
		{
			name:        "raw seat object with no envelope",
			stdout:      `{"verdict":"raw","findings":["x"]}`,
			wantOK:      true,
			wantVerdict: "raw",
			wantWarnSub: "not a claude JSON envelope",
		},
		{
			name:        "envelope is_error true degrades to not-ok",
			stdout:      `{"type":"result","is_error":true,"total_cost_usd":0.2,"result":"{\"verdict\":\"boom\"}"}`,
			wantOK:      false,
			wantVerdict: "boom",
			wantWarnSub: "is_error",
		},
		{
			name:        "garbage with no json object",
			stdout:      `total nonsense, no braces here`,
			wantOK:      false, // no usable object → degraded, not OK
			wantWarnSub: "no JSON object",
		},
		{
			name:        "empty output",
			stdout:      ``,
			wantWarnSub: "no JSON object",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseSeatOutput("critic", tt.stdout)
			if got.Role != "critic" {
				t.Fatalf("role = %q, want critic", got.Role)
			}
			if got.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q", got.Verdict, tt.wantVerdict)
			}
			if got.GoalMet != tt.wantGoalMet {
				t.Errorf("goalMet = %v, want %v", got.GoalMet, tt.wantGoalMet)
			}
			if tt.wantFindings > 0 && len(got.Findings) != tt.wantFindings {
				t.Errorf("findings = %d, want %d", len(got.Findings), tt.wantFindings)
			}
			if tt.wantWarnSub != "" && !hasWarning(got.Warnings, tt.wantWarnSub) {
				t.Errorf("warnings %v, want one containing %q", got.Warnings, tt.wantWarnSub)
			}
			if got.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (warnings=%v)", got.OK, tt.wantOK, got.Warnings)
			}
		})
	}
}

func TestParseSeatOutput_BacktickInValuePreserved(t *testing.T) {
	t.Parallel()
	// A valid seat object whose value literally contains a ```json fence must
	// parse correctly: balanced-object extraction runs on the raw text before any
	// fence stripping, so the value is preserved rather than sliced apart.
	stdout := "{\"proposal\":\"see ```json``` here\",\"verdict\":\"ok\"}"
	got := ParseSeatOutput("build", stdout)
	if got.Verdict != "ok" {
		t.Errorf("verdict = %q, want ok", got.Verdict)
	}
	if got.Proposal != "see ```json``` here" {
		t.Errorf("proposal = %q, want %q", got.Proposal, "see ```json``` here")
	}
	if hasWarning(got.Warnings, "no JSON object") || hasWarning(got.Warnings, "not valid JSON") {
		t.Errorf("object with backticks in a value should parse cleanly; warnings=%v", got.Warnings)
	}
}

func TestParseSeatOutput_CostAndTurns(t *testing.T) {
	t.Parallel()
	got := ParseSeatOutput("measure", `{"type":"result","total_cost_usd":1.5,"num_turns":7,"result":"{\"metrics\":{\"progress\":0.4,\"risk\":0.2}}"}`)
	if got.CostUSD != 1.5 {
		t.Errorf("cost = %v, want 1.5", got.CostUSD)
	}
	if got.NumTurns != 7 {
		t.Errorf("turns = %d, want 7", got.NumTurns)
	}
	if got.Metrics["progress"] != 0.4 || got.Metrics["risk"] != 0.2 {
		t.Errorf("metrics = %v, want progress=0.4 risk=0.2", got.Metrics)
	}
}

func TestParseSeatOutput_NonNumericMetricDropped(t *testing.T) {
	t.Parallel()
	// progress is a number, risk is a string: the lenient path keeps progress and
	// drops risk rather than losing the whole object.
	got := ParseSeatOutput("measure", `{"type":"result","total_cost_usd":0.1,"result":"{\"metrics\":{\"progress\":0.9,\"risk\":\"high\"},\"verdict\":\"kept\"}"}`)
	if got.Verdict != "kept" {
		t.Errorf("verdict = %q, want kept", got.Verdict)
	}
	if got.Metrics["progress"] != 0.9 {
		t.Errorf("progress = %v, want 0.9", got.Metrics["progress"])
	}
	if _, ok := got.Metrics["risk"]; ok {
		t.Errorf("risk should have been dropped, got %v", got.Metrics["risk"])
	}
}

func TestExtractOuterJSONObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"a":1}`, `{"a":1}`, true},
		{`prefix {"a":{"b":2}} suffix`, `{"a":{"b":2}}`, true},
		{`{"s":"a}b"}`, `{"s":"a}b"}`, true},     // brace inside string
		{`{"s":"a\"}b"}`, `{"s":"a\"}b"}`, true}, // escaped quote then brace
		{`no object`, ``, false},
		{`{"unbalanced":`, ``, false},
	}
	for _, tt := range tests {
		got, ok := extractOuterJSONObject(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("extractOuterJSONObject(%q) = (%q,%v), want (%q,%v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestStripJSONFences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{`{"a":1}`, `{"a":1}`},
	}
	for _, tt := range tests {
		if got := stripJSONFences(tt.in); got != tt.want {
			t.Errorf("stripJSONFences(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
