//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import (
	"errors"
	"os/exec"
	"syscall"
)

func detachDaemon(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}

func interruptedExit(err error) bool {
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGINT
}
