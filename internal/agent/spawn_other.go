//go:build !unix

package agent

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"
)

// reapGrace bounds the wait for cmd.Wait after Kill on non-unix. See the unix
// build's spawn.go for the rationale; an unbounded wait would hang the round if
// the child cannot be reaped.
const reapGrace = 2 * time.Second

// lockedBuffer is a mutex-guarded cappedBuffer. On the reap-failure bail we
// abandon cmd.Wait, but exec's internal stdout/stderr copy goroutines may still
// be writing; the lock makes the concurrent Write / String read race-free.
type lockedBuffer struct {
	mu  sync.Mutex
	buf cappedBuffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.exceeded
}

// runWithReaping is the non-unix fallback. Process groups are a POSIX concept;
// here we enforce the timeout and kill the single process on expiry, with the
// same bounded post-kill reap grace as unix. The loop plugin targets unix
// (darwin/linux); this exists only so the package builds everywhere.
func runWithReaping(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, meta ...*runResultMeta) (stdout, stderr string, timedOut bool, err error) {
	outBuf := &lockedBuffer{buf: newCappedBuffer(MaxOutputBytes)}
	errBuf := &lockedBuffer{buf: newCappedBuffer(MaxStderrBytes)}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	var reapFailed bool
	defer func() {
		if len(meta) > 0 && meta[0] != nil {
			*meta[0] = runResultMeta{
				stdoutTruncated: outBuf.exceeded(),
				stderrTruncated: errBuf.exceeded(),
				reapFailed:      reapFailed,
			}
		}
	}()

	if err = cmd.Start(); err != nil {
		return "", "", false, err
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case werr := <-done:
		return outBuf.String(), errBuf.String(), false, werr
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		reap := time.NewTimer(reapGrace)
		defer reap.Stop()
		select {
		case <-done:
			return outBuf.String(), errBuf.String(), timedOut, runCtx.Err()
		case <-reap.C:
			reapFailed = true
			return outBuf.String(), errBuf.String(), timedOut, runCtx.Err()
		}
	}
}
