//go:build linux

package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureQobuzCancellation(command *exec.Cmd) {
	// Stop streamrip's children too; an orphan converter must not keep writing
	// after its job's private staging directory has been cleaned.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
}
