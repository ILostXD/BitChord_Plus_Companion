//go:build !linux

package main

import (
	"os/exec"
	"time"
)

func configureQobuzCancellation(command *exec.Cmd) { command.WaitDelay = 5 * time.Second }
