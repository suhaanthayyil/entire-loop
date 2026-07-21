//go:build !unix

package agent

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// runWithReaping is the non-unix fallback. Process groups are a POSIX concept;
// here we enforce the timeout and kill the single process on expiry. The loop
// plugin targets unix (darwin/linux); this exists only so the package builds
// everywhere.
func runWithReaping(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (stdout, stderr string, timedOut bool, err error) {
	outBuf := newCappedBuffer(MaxOutputBytes)
	errBuf := newCappedBuffer(MaxStderrBytes)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

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
		<-done
		return outBuf.String(), errBuf.String(), timedOut, runCtx.Err()
	}
}
