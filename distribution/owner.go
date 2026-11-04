package distribution

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/value"
)

const (
	directoryReadBatch       = 256
	privateDirectoryAttempts = 32
	stagingDirectoryPrefix   = ".stage-"
	retiringDirectoryPrefix  = ".retiring-"
)

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
	if err := s.reclaimPrivate(); err != nil {
		return err
	}
	return s.lock.Sync()
}

// reclaimPrivate removes staging and retiring directories left by a previous
// Store. The directory lock proves that no such Store still owns them, and a
// public release namespace is never touched.
func (s *Store) reclaimPrivate() (err error) {
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
			if !strings.HasPrefix(entry.Name(), stagingDirectoryPrefix) && !strings.HasPrefix(entry.Name(), retiringDirectoryPrefix) {
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
			return nil
		}
	}
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

// storePhase is the Store's lifecycle. Draining refuses new work while granted
// Leases finish; releasing closes the root and gives up the directory lock.
type storePhase uint8

const (
	storeOpen storePhase = iota
	storeDraining
	storeReleasing
)

// Closing cannot revoke I/O already promised to an admitted Lease.
type ioOrigin uint8

const (
	newOperation ioOrigin = iota
	leaseOperation
)

func (s *Store) beginOperation(origin ioOrigin) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == storeReleasing || s.phase == storeDraining && origin == newOperation {
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

// The Store owns shutdown independently of every caller waiting for it. A
// cancelled wait must not strand the original directory lock after leases exit.
func (s *Store) Close(ctx context.Context) error {
	if ctx == nil {
		return frame.ErrInvalid
	}
	s.mu.Lock()
	if s.phase == storeOpen {
		s.phase = storeDraining
		go s.closeOriginal()
	}
	done := s.closeDone
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return cancellation.Err(ctx)
	case <-done:
		return s.closeErr
	}
}

func (s *Store) closeOriginal() {
	for {
		s.mu.Lock()
		if s.operations != 0 || len(s.leases) != 0 {
			changed := s.changed
			s.mu.Unlock()
			<-changed
			continue
		}
		s.phase = storeReleasing
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
		return
	}
}

func (s *Store) openRelease(digest value.Digest) (*os.Root, error) {
	name := digest.String()
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&(fs.ModeSymlink|specialFileModes) != 0 || info.Mode().Perm() != sealedDirectoryMode {
		return nil, ErrIntegrity
	}
	root, err := s.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) || opened.Mode() != info.Mode() {
		return nil, errors.Join(ErrIntegrity, err, root.Close())
	}
	return root, nil
}

func (s *Store) privateDirectory(prefix string) (string, *os.Root, fs.FileInfo, error) {
	for range privateDirectoryAttempts {
		name := prefix + rand.Text()
		if err := s.root.Mkdir(name, privateDirectoryMode); errors.Is(err, os.ErrExist) {
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
			return root.Chmod(path, privateDirectoryMode)
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
