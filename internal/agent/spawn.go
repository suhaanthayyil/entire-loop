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

// reapGrace bounds how long we wait for cmd.Wait AFTER SIGKILL. A normal child
// dies and is reaped well within this; only a child stuck in uninterruptible
// sleep (D-state) survives SIGKILL, and an UNBOUNDED wait there would hang the
// whole round forever. On expiry we abandon cmd.Wait to a detached goroutine (the
// `done` channel is buffered size-1, so its eventual send never blocks and nothing
// leaks) and return best-effort output with a reap warning.
const reapGrace = 2 * time.Second

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
// The optional meta out-param (variadic pointer so existing 3-arg callers and the
// 4-value positional destructuring are untouched) receives out-of-band details:
// stdout/stderr truncation and whether the group could not be reaped after SIGKILL.
//
// stdout/stderr are wired through explicit os.Pipe files we own (not the capped
// buffers directly). Assigning an *os.File to cmd.Stdout makes exec hand the fd
// straight to the child with NO internal copy goroutine — and exec's copy
// goroutines only join once EVERY write end is closed, so a lingering grandchild
// holding the fd would otherwise make cmd.Wait block until the timeout. We read
// the pipes ourselves and, once the process exits, apply a bounded read deadline
// so such a grandchild can never keep us blocked.
func runWithReaping(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, meta ...*runResultMeta) (stdout, stderr string, timedOut bool, err error) {
	outBuf := newCappedBuffer(MaxOutputBytes)
	errBuf := newCappedBuffer(MaxStderrBytes)

	// reapFailed is threaded to the deferred meta writer below. The defer runs
	// after the return values are set — i.e. after finish/finishDetached has done
	// readWG.Wait() — so outBuf/errBuf.exceeded are final and race-free by then.
	var reapFailed bool
	defer func() {
		if len(meta) > 0 && meta[0] != nil {
			*meta[0] = runResultMeta{
				stdoutTruncated: outBuf.exceeded,
				stderrTruncated: errBuf.exceeded,
				reapFailed:      reapFailed,
			}
		}
	}()

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
	// been reaped) by the time finish runs.
	finish := func(werr error, timedOut bool) (string, string, bool, error) {
		deadline := time.Now().Add(postExitReadGrace)
		_ = outR.SetReadDeadline(deadline)
		_ = errR.SetReadDeadline(deadline)
		readWG.Wait()
		_ = outR.Close()
		_ = errR.Close()
		return outBuf.String(), errBuf.String(), timedOut, werr
	}

	// finishDetached is the reap-failure bail: cmd.Wait is left to its (already
	// running) goroutine. The live child still holds the pipe write fds, so an
	// unbounded readWG.Wait would block — we set an immediate read deadline to
	// unblock io.Copy, join the readers, and return best-effort captured output.
	finishDetached := func(werr error, timedOut bool) (string, string, bool, error) {
		now := time.Now()
		_ = outR.SetReadDeadline(now)
		_ = errR.SetReadDeadline(now)
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
			return finish(runCtx.Err(), timedOut)
		case <-t.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			reap := time.NewTimer(reapGrace)
			defer reap.Stop()
			select {
			case <-done:
				return finish(runCtx.Err(), timedOut)
			case <-reap.C:
				reapFailed = true
				return finishDetached(runCtx.Err(), timedOut)
			}
		}
	}
}
