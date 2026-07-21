//go:build unix

package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunWithReaping_CapturesStdout is a basic sanity check that the pipe
// plumbing captures a worker's stdout and reaps a normal exit.
func TestRunWithReaping_CapturesStdout(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sh", "-c", "printf hello")
	stdout, _, timedOut, err := runWithReaping(context.Background(), cmd, 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if timedOut {
		t.Fatalf("did not expect a timeout")
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Errorf("stdout = %q, want hello", stdout)
	}
}

// TestRunWithReaping_LingeringGrandchildDoesNotStall is the finding-11 guard: a
// grandchild that inherits stdout and lingers far longer than the worker must NOT
// keep runWithReaping blocked (old behavior: exec's copy goroutine + cmd.Wait
// join only when EVERY write end closes, so this would block ~10s). We own the
// pipes and apply a post-exit read deadline, so it returns shortly after the
// worker itself exits.
func TestRunWithReaping_LingeringGrandchildDoesNotStall(t *testing.T) {
	t.Parallel()
	// The `sleep 10` is backgrounded and inherits stdout, holding the write fd
	// open long after the parent `sh` prints and exits.
	cmd := exec.Command("sh", "-c", "sleep 10 & printf done")

	start := time.Now()
	stdout, _, timedOut, err := runWithReaping(context.Background(), cmd, 60*time.Second)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if timedOut {
		t.Fatalf("did not expect a timeout")
	}
	if strings.TrimSpace(stdout) != "done" {
		t.Errorf("stdout = %q, want done", stdout)
	}
	// Must return promptly after exit (post-exit grace is ~2s), NOT wait out the
	// lingering grandchild (~10s) or the 60s timeout.
	if elapsed > 6*time.Second {
		t.Errorf("runWithReaping stalled %s on a lingering grandchild; want < 6s", elapsed)
	}
}

// TestRunWithReaping_TimeoutReapsAndReports verifies a worker that exceeds the
// wall-clock budget is killed and reported as timed out, promptly.
func TestRunWithReaping_TimeoutReapsAndReports(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sh", "-c", "sleep 30")

	start := time.Now()
	_, _, timedOut, _ := runWithReaping(context.Background(), cmd, 200*time.Millisecond)
	elapsed := time.Since(start)

	if !timedOut {
		t.Errorf("expected timedOut=true")
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout path took %s; want prompt kill", elapsed)
	}
}
