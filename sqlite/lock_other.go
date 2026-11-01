//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package sqlite

import (
	"errors"
	"os"
)

func acquireAuthority(string) (*os.File, error) {
	return nil, errors.New("sqlite: this platform has no supported authority lock")
}

func releaseAuthority(file *os.File) error { return file.Close() }
