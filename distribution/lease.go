package distribution

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type Lease struct {
	store   *Store
	release Release
	mu      sync.Mutex
	closed  bool
}

func (s *Store) Acquire(ctx context.Context, digest value.Digest) (_ *Lease, err error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return nil, err
	}
	defer done()
	if digest.Validate() != nil {
		return nil, ErrInvalid
	}
	// Claim the content before reading it. The gate protects this claim and
	// collection; hashing the immutable tree does not block unrelated admission.
	lease := &Lease{store: s, release: Release{Digest: digest}}
	if err := s.admission.Under(ctx, func() error {
		s.mu.Lock()
		s.leases[digest]++
		s.mu.Unlock()
		return nil
	}); err != nil {
		return nil, err
	}
	release, err := s.inspect(ctx, digest)
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	lease.release = release
	return lease, nil
}

func (l *Lease) Read(ctx context.Context, name string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, frame.ErrClosed
	}
	return l.store.readArtifact(ctx, l.release, name)
}

// Executable returns only a verified executable in the immutable leased tree.
// The lease must remain held through creation and the execution's protection.
func (l *Lease) Executable(ctx context.Context, name string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return "", frame.ErrClosed
	}
	var executable bool
	for _, artifact := range l.release.Manifest.Artifacts {
		if artifact.Path == name {
			executable = artifact.Executable
			break
		}
	}
	if !executable {
		return "", ErrInvalid
	}
	if _, err := l.store.readArtifact(ctx, l.release, name); err != nil {
		return "", err
	}
	return filepath.Join(l.store.directory, l.release.Digest.String(), filepath.FromSlash(name)), nil
}

func (l *Lease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	l.store.leases[l.release.Digest]--
	if l.store.leases[l.release.Digest] == 0 {
		delete(l.store.leases, l.release.Digest)
	}
	l.store.notifyLocked()
	return nil
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

func (s *Store) readArtifact(ctx context.Context, release Release, name string) ([]byte, error) {
	done, err := s.beginOperation(leaseOperation)
	if err != nil {
		return nil, err
	}
	defer done()
	var found *Artifact
	for _, artifact := range release.Manifest.Artifacts {
		if artifact.Path == name {
			copy := artifact
			found = &copy
			break
		}
	}
	if found == nil {
		return nil, ErrInvalid
	}
	root, err := s.openRelease(release.Digest)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	content, err := readRegular(ctx, root, name, found.Size)
	if err != nil {
		return nil, err
	}
	if err := found.verify(content); err != nil {
		return nil, err
	}
	return content.data, nil
}
