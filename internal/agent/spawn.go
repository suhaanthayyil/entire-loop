//go:build unix

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// killGrace is how long we wait after SIGTERM to the process group before
// escalating to SIGKILL.
const killGrace = 5 * time.Second

// postExitReadGrace bounds how long we keep draining a worker's stdout/stderr
// after the process itself has exited. A well-behaved worker closes its pipes on
// exit and we see EOF immediately; a grandchild (node/claude) that lingers with a
// dup'd write fd would otherwise hold the pipe open, so after this grace we set a
// read deadline and stop. This is what keeps cmd.Wait / the readers from stalling
// up to the full worker timeout on an otherwise-fast success.
const postExitReadGrace = 2 * time.Second

// runWithReaping starts cmd in its own process group, enforces a wall-clock
// timeout, and on timeout or cancellation kills the WHOLE process group
// (SIGTERM then SIGKILL after a grace) so a worker's node/claude grandchildren
// are reaped — the Go equivalent of `pkill -P`. It returns the captured stdout
// and stderr (each bounded), whether the run timed out, and the wait error.
//
// stdout/stderr are wired through explicit os.Pipe files we own (not the capped
// buffers directly). Assigning an *os.File to cmd.Stdout makes exec hand the fd
// straight to the child with NO internal copy goroutine — and exec's copy
// goroutines only join once EVERY write end is closed, so a lingering grandchild
// holding the fd would otherwise make cmd.Wait block until the timeout. We read
// the pipes ourselves and, once the process exits, apply a bounded read deadline
// so such a grandchild can never keep us blocked.
func runWithReaping(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (stdout, stderr string, timedOut bool, err error) {
	outBuf := newCappedBuffer(MaxOutputBytes)
	errBuf := newCappedBuffer(MaxStderrBytes)

	outR, outW, err := os.Pipe()
	if err != nil {
		return "", "", false, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return "", "", false, err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Put the child in its own process group so we can signal the whole tree.
	cmd.SysProcAttr.Setpgid = true

	if err = cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return "", "", false, err
	}
	// The child holds its own dup of the write ends; the parent must drop its
	// copies so a closed child fd is observable as EOF by our readers.
	_ = outW.Close()
	_ = errW.Close()

	var readWG sync.WaitGroup
	readWG.Add(2)
	go func() { defer readWG.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer readWG.Done(); _, _ = io.Copy(&errBuf, errR) }()

	// With Setpgid and no Pgid set, the child becomes its own group leader, so
	// the group id equals the child pid. Signaling the negative pid hits the group.
	pgid := cmd.Process.Pid

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// finish drains the readers within a bounded post-exit grace, then closes the
	// read ends and returns the captured output. The process has already exited (or
	// been killed) by the time finish runs.
	finish := func(werr error, timedOut bool) (string, string, bool, error) {
		deadline := time.Now().Add(postExitReadGrace)
		_ = outR.SetReadDeadline(deadline)
		_ = errR.SetReadDeadline(deadline)
		readWG.Wait()
		_ = outR.Close()
		_ = errR.Close()
		return outBuf.String(), errBuf.String(), timedOut, werr
	}

	select {
	case werr := <-done:
		return finish(werr, false)
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		t := time.NewTimer(killGrace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
		return finish(runCtx.Err(), timedOut)
	}
}
