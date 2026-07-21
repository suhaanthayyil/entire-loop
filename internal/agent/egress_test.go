package agent

import "testing"

func TestRejectForNoEgress(t *testing.T) {
	tests := []struct {
		name      string
		envVar    string // set to "1" when non-empty
		agent     string
		wantError bool
	}{
		{"no-egress off: claude allowed", "", "claude", false},
		{"NO_EGRESS on: claude rejected", "ENTIRE_BRAIN_NO_EGRESS", "claude", true},
		{"NO_EGRESS on: claude-code rejected", "ENTIRE_BRAIN_NO_EGRESS", "claude-code", true},
		{"NO_EGRESS on: ollama allowed", "ENTIRE_BRAIN_NO_EGRESS", "ollama", false},
		{"LOCAL_ONLY on: claude rejected", "ENTIRE_BRAIN_LOCAL_ONLY", "claude", true},
		{"LOCAL_ONLY on: ollama allowed", "ENTIRE_BRAIN_LOCAL_ONLY", "ollama", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv forbids t.Parallel; each case gets a clean env.
			t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
			t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
			if tt.envVar != "" {
				t.Setenv(tt.envVar, "1")
			}
			err := RejectForNoEgress(tt.agent)
			if (err != nil) != tt.wantError {
				t.Fatalf("RejectForNoEgress(%q) error = %v, wantError = %v", tt.agent, err, tt.wantError)
			}
		})
	}
}

func TestNoEgressMode_TruthyValues(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on", "TRUE", "On"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
			t.Setenv("ENTIRE_BRAIN_NO_EGRESS", v)
			if !NoEgressMode() {
				t.Errorf("NoEgressMode() = false for %q, want true", v)
			}
		})
	}
	t.Run("falsey", func(t *testing.T) {
		t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "0")
		t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
		if NoEgressMode() {
			t.Error("NoEgressMode() = true for 0, want false")
		}
	})
}
