package distribution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

const directoryReadBatch = 256
const privateDirectoryAttempts = 32

func (s *Store) openOwner() (err error) {
	s.root, err = os.OpenRoot(s.directory)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		if s.lock != nil {
			err = errors.Join(err, unlockStore(s.lock))
		}
		err = errors.Join(err, s.root.Close())
	}()
	s.identity, err = s.root.Stat(".")
	if err != nil {
		return err
	}
	s.lock, err = lockStore(s.root)
	if err != nil {
		return err
	}
	if err := s.validateDirectory(); err != nil {
		return err
	}
	// The directory lock proves that no previous Store can still own these
	// private operations. Recovery never removes a public release namespace.
	directory, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	for {
		entries, readErr := directory.ReadDir(directoryReadBatch)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".stage-") && !strings.HasPrefix(entry.Name(), ".retiring-") {
				continue
			}
			info, err := s.root.Lstat(entry.Name())
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
				return ErrIntegrity
			}
			if err := s.removePrivate(entry.Name(), info); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return s.lock.Sync()
}

// validateDirectory detects replacement of any canonical parent or the root.
// All actual I/O is additionally relative to the original held root, so a
// replacement between this check and an operation cannot redirect it outside.
func (s *Store) validateDirectory() error {
	info, err := os.Lstat(s.directory)
	if err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !os.SameFile(s.identity, info) {
		return ErrIntegrity
	}
	canonical, err := filepath.EvalSymlinks(s.directory)
	if err != nil || canonical != s.directory {
		return errors.Join(ErrIntegrity, err)
	}
	return nil
}

// beginOperation reserves real directory I/O before it starts. Existing lease
// reads remain possible while Close joins them; new claims and mutations seal.
func (s *Store) beginOperation(existing bool) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeAttempted || s.closing && !existing {
		return nil, frame.ErrClosed
	}
	if err := s.validateDirectory(); err != nil {
		return nil, err
	}
	s.operations++
	return func() {
		s.mu.Lock()
		s.operations--
		s.notifyLocked()
		s.mu.Unlock()
	}, nil
}

func (s *Store) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Close seals new work and waits for original I/O and leases. A cancelled wait
// leaves that close in progress; it neither unlocks the directory nor abandons
// claims. The original descriptors are released once, without retrying errors.
func (s *Store) Close(ctx context.Context) error {
	if ctx == nil {
		return frame.ErrInvalid
	}
	for {
		s.mu.Lock()
		s.closing = true
		if s.closeAttempted {
			done := s.closeDone
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				s.mu.Lock()
				err := s.closeErr
				s.mu.Unlock()
				return err
			}
		}
		if s.operations != 0 || len(s.leases) != 0 {
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
				continue
			}
		}
		s.closeAttempted = true
		s.mu.Unlock()
		err := s.root.Close()
		if err == nil {
			err = unlockStore(s.lock)
		}
		s.mu.Lock()
		s.closeErr = err
		close(s.closeDone)
		s.notifyLocked()
		s.mu.Unlock()
		return err
	}
}

func (s *Store) openRelease(digest value.Digest) (*os.Root, error) {
	name := digest.String()
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm() != 0500 {
		return nil, ErrIntegrity
	}
	root, err := s.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0500 {
		return nil, errors.Join(ErrIntegrity, err, root.Close())
	}
	return root, nil
}

func (s *Store) privateDirectory(prefix string) (string, *os.Root, fs.FileInfo, error) {
	for range privateDirectoryAttempts {
		var entropy [16]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return "", nil, nil, err
		}
		name := prefix + hex.EncodeToString(entropy[:])
		if err := s.root.Mkdir(name, 0700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", nil, nil, err
		}
		info, err := s.root.Lstat(name)
		if err != nil {
			return "", nil, nil, err
		}
		root, err := s.root.OpenRoot(name)
		if err != nil {
			return "", nil, nil, errors.Join(err, s.root.Remove(name))
		}
		opened, err := root.Stat(".")
		if err != nil || !info.IsDir() || !os.SameFile(info, opened) {
			return "", nil, nil, errors.Join(ErrIntegrity, err, root.Close())
		}
		return name, root, info, nil
	}
	return "", nil, nil, ErrCapacity
}

func (s *Store) removePrivate(name string, original fs.FileInfo) error {
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !os.SameFile(original, info) {
		return ErrIntegrity
	}
	root, err := s.root.OpenRoot(name)
	if err != nil {
		return err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(original, opened) {
		return errors.Join(ErrIntegrity, err, root.Close())
	}
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return root.Chmod(path, 0700)
		}
		return nil
	})
	err = errors.Join(err, root.Close())
	if err != nil {
		return err
	}
	info, err = s.root.Lstat(name)
	if err != nil || !os.SameFile(original, info) {
		return errors.Join(ErrIntegrity, err)
	}
	return s.root.RemoveAll(name)
}
