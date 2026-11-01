//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package runner

import (
	"errors"
	"os/exec"
)

func detachDaemon(*exec.Cmd) error {
	return errors.New("runner: daemon isolation is unsupported on this platform")
}

func interruptedExit(error) bool { return false }
