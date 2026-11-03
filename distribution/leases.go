package distribution

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/value"
)

func (s *Store) ImportDirectory(ctx context.Context, directory string) (Release, error) {
	lease, err := s.ImportDirectoryHeld(ctx, directory)
	if err != nil {
		return Release{}, err
	}
	return lease.Snapshot(), lease.Close()
}

func (s *Store) ImportArchive(ctx context.Context, fileName string, expected value.Digest) (Release, error) {
	lease, err := s.ImportArchiveHeld(ctx, fileName, expected)
	if err != nil {
		return Release{}, err
	}
	return lease.Snapshot(), lease.Close()
}

// Snapshot is a detached immutable declaration projection. Retaining it does not
// retain content; the original Lease owns that claim until Close.
func (l *Lease) Snapshot() Release {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.release.snapshot()
}

// Directory permits a trusted profile adapter to open the exact admitted tree.
// Keep this lease until every object that may use the directory has exited.
// Directory is not a writable data root or proof that a process has exited.
func (l *Lease) Directory() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return "", frame.ErrClosed
	}
	return l.store.Path(l.release.Digest)
}

// Path projects an immutable release location for diagnostics and declarations.
// It performs no admission: actual reads and execution require an acquired Lease.
func (s *Store) Path(digest value.Digest) (string, error) {
	done, err := s.beginOperation(leaseOperation)
	if err != nil {
		return "", err
	}
	defer done()
	if digest.Validate() != nil {
		return "", ErrInvalid
	}
	return filepath.Join(s.directory, digest.String()), nil
}

// Digests observes the content namespace. Each later Remove still checks the
// original retention facts and live claims under the shared admission gate.
func (s *Store) Digests(ctx context.Context) (_ []value.Digest, err error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return nil, err
	}
	defer done()
	directory, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	var result []value.Digest
	for {
		if err := cancellation.Err(ctx); err != nil {
			return nil, err
		}
		entries, readErr := directory.ReadDir(directoryReadBatch)
		for _, entry := range entries {
			if digest, err := value.ParseDigest(entry.Name()); err == nil {
				result = append(result, digest)
			}
		}
		if errors.Is(readErr, io.EOF) {
			slices.SortFunc(result, value.Digest.Compare)
			return result, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}
