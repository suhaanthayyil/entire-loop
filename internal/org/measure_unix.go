//go:build unix

package org

import (
	"os/exec"
	"syscall"
)

// configureMeasureProcGroup puts the measure command in its OWN process group and
// wires Cancel to kill that whole group. When the command's context expires,
// exec.Cmd calls Cancel, so a forking grandchild (which inherits the group) is
// reaped along with the shell instead of surviving to hold stdout open.
func configureMeasureProcGroup(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
	c.Cancel = func() error { return killMeasureProcGroup(c) }
}

// killMeasureGroup force-kills the command's process group (used on the over-cap
// path, where we stop reading before the command has finished).
func killMeasureGroup(c *exec.Cmd) { _ = killMeasureProcGroup(c) }

// killMeasureProcGroup SIGKILLs the whole process group. A negative pid targets the
// group leader and every process in it (the shell plus anything it forked).
func killMeasureProcGroup(c *exec.Cmd) error {
	if c.Process == nil {
		return nil
	}
	return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
}
