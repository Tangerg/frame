//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package sqlite

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func acquireAuthority(path string) (*os.File, error) {
	// On Darwin, flock on the database inode conflicts with SQLite's byte-range
	// locks. Keep the stable sidecar after release so normal opens share one lock.
	file, err := openAuthorityFile(path + ".frame-lock")
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("sqlite: authority already held: %w", err), file.Close())
	}
	// Even closing another database descriptor can release the process's SQLite
	// locks. A refused authority must never open the live owner's database inode.
	database, err := openAuthorityFile(path)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := database.Close(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func releaseAuthority(file *os.File) error {
	return file.Close()
}

func openAuthorityFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, errors.Join(errors.New("sqlite: database authority refuses non-regular or hard-linked files"), file.Close())
	}
	return file, nil
}
