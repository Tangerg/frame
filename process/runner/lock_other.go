//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package runner

import (
	"errors"
	"os"
)

func verifyAuthorityDirectory(string) error {
	return errors.New("runner: unsupported authority directory")
}
func lockDirectory(*os.File) error {
	return errors.New("runner: this platform has no supported execution authority lock")
}
func unlockDirectory(file *os.File) error { return file.Close() }
