//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package distribution

import (
	"errors"
	"os"
)

func lockStore(*os.Root) (*os.File, error) {
	return nil, errors.New("distribution: this platform has no supported directory authority lock")
}
func unlockStore(file *os.File) error { return file.Close() }
