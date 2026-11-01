//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package distribution

import (
	"errors"
	"os"
	"syscall"

	"github.com/Tangerg/frame"
)

// Lock the actual directory inode, not a replaceable child lock-file name.
func lockStore(root *os.Root) (*os.File, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(frame.ErrInUse, err, file.Close())
	}
	return file, nil
}

func unlockStore(file *os.File) error {
	// Closing the original descriptor releases its flock in the same physical
	// operation. Do not expose an unlocked but still live directory handle.
	return file.Close()
}
