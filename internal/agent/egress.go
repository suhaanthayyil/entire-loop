package agent

import (
	"fmt"
	"os"
	"strings"
)

// NoEgressMode reports whether the brain's local-only mode is enabled via env.
// It mirrors the entire-brain / entire-judge contract so a loop honors the same
// no-egress switch as the rest of the system.
func NoEgressMode() bool {
	return envBool("ENTIRE_BRAIN_NO_EGRESS") || envBool("ENTIRE_BRAIN_LOCAL_ONLY")
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// RejectForNoEgress returns an error when no-egress mode is on and the requested
// worker agent can send selected brain context outside the local loopback. Only
// local-loopback backends (ollama) are allowed under no-egress.
func RejectForNoEgress(agent string) error {
	if !NoEgressMode() {
		return nil
	}
	switch agent {
	case "claude", "claude-code", "codex", "command", "auto":
		return fmt.Errorf("no_egress: worker agent %q can send selected brain context outside local loopback; "+
			"unset ENTIRE_BRAIN_NO_EGRESS/ENTIRE_BRAIN_LOCAL_ONLY (no local-loopback worker is implemented yet)", agent)
	default:
		// A local-loopback backend (e.g. ollama) would be allowed under no-egress —
		// but no local worker exists yet, so `run` hard-selects "claude" and fails
		// fast above. This branch is here for when one lands.
		// TODO(phase-b): implement a local ollama worker so no-egress runs can proceed.
		return nil
	}
}
