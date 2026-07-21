//go:build unix

package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunWithReaping_MetaTruncation verifies the capped-buffer overflow flag is
// read back through the meta out-param: a worker that prints more than the cap is
// reported as stdoutTruncated so the runner can warn instead of silently feeding a
// sliced envelope to the parser.
func TestRunWithReaping_MetaTruncation(t *testing.T) {
	t.Parallel()
	// Emit well over MaxOutputBytes so the capped buffer overflows.
	cmd := exec.Command("sh", "-c", "yes AAAAAAAA | head -c 400000")
	var meta runResultMeta
	stdout, _, timedOut, err := runWithReaping(context.Background(), cmd, 30*time.Second, &meta)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if timedOut {
		t.Fatalf("did not expect a timeout")
	}
	if !meta.stdoutTruncated {
		t.Errorf("expected stdoutTruncated=true for an over-cap worker")
	}
	if len(stdout) > MaxOutputBytes {
		t.Errorf("captured stdout %d exceeds cap %d", len(stdout), MaxOutputBytes)
	}
}

// TestRunWithReaping_NoTruncationForSmallOutput is the negative control: a small
// output must not be reported as truncated.
func TestRunWithReaping_NoTruncationForSmallOutput(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sh", "-c", "printf hello")
	var meta runResultMeta
	stdout, _, _, err := runWithReaping(context.Background(), cmd, 10*time.Second, &meta)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if meta.stdoutTruncated {
		t.Errorf("small output must not be flagged truncated")
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Errorf("stdout = %q, want hello", stdout)
	}
	if meta.reapFailed {
		t.Errorf("a clean exit must not flag reapFailed")
	}
}
