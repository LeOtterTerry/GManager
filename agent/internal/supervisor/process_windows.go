//go:build windows

package supervisor

import (
	"errors"
	"os/exec"
)

// Windows process control does not create a Job Object. That object is
// untested here because there is no Plains of Pain install in CI. See
// docs/UNVERIFIED.md. Kill is TerminateProcess. Interrupt is not a
// confirmed clean stop and is not sent.

func prepareCommand(*exec.Cmd) {}

func interruptProcess(*exec.Cmd) error {
	return errors.New("interrupt stop is not available on Windows; a clean Plains of Pain stop is unverified")
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
