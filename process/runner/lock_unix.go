//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const nonOwnerWritePermissions os.FileMode = 0022

// A writable ancestor can replace the private directory and its authenticated socket.
// A sticky root-owned temporary directory protects the next component by its owner.
func verifyAuthorityDirectory(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() {
			return errors.New("runner: invalid authority directory")
		}
		uid := int(stat.Uid)
		if uid != os.Geteuid() && (current == path || uid != 0) {
			return fmt.Errorf("runner: untrusted directory owner: %s", current)
		}
		if info.Mode().Perm()&nonOwnerWritePermissions != 0 && (current == path || info.Mode()&os.ModeSticky == 0) {
			return fmt.Errorf("runner: writable authority directory: %s", current)
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func lockDirectory(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockDirectory(file *os.File) error {
	return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close())
}
