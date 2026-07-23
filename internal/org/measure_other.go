//go:build !unix

package org

import "os/exec"

// configureMeasureProcGroup is a no-op on platforms without POSIX process groups.
// The default context cancel (which kills the process) plus the WaitDelay grace
// still bound the wait; there is no portable group-kill to install here.
func configureMeasureProcGroup(c *exec.Cmd) { _ = c }

// killMeasureGroup best-effort kills the single process (no group semantics).
func killMeasureGroup(c *exec.Cmd) {
	if c.Process != nil {
		_ = c.Process.Kill()
	}
}
