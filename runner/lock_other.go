//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package runner

import (
	"errors"
	"os"
)

func lockDirectory(string) (*os.File, error) {
	return nil, errors.New("runner: this platform has no supported execution authority lock")
}
func unlockDirectory(file *os.File) error { return file.Close() }

func openExecutable(path string) (*os.File, error) { return os.Open(path) }
