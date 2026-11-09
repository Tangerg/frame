package distribution

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

// Copies retain the original claim; only that claim can release content.
type Lease struct{ claim *leaseClaim }

type leaseClaim struct {
	mu      sync.Mutex
	store   *Store
	release Release
}

func (s *Store) claimRelease(release Release) *Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases[release.Digest]++
	return &Lease{claim: &leaseClaim{store: s, release: release}}
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
	var lease *Lease
	if err := s.admission.Under(ctx, func() error {
		lease = s.claimRelease(Release{Digest: digest})
		return nil
	}); err != nil {
		return nil, err
	}
	release, err := s.inspect(ctx, digest)
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	lease.claim.release = release
	return lease, nil
}

func (l *Lease) Read(ctx context.Context, name string) ([]byte, error) {
	if l == nil || l.claim == nil {
		return nil, frame.ErrInvalid
	}
	claim := l.claim
	claim.mu.Lock()
	defer claim.mu.Unlock()
	if claim.store == nil {
		return nil, frame.ErrClosed
	}
	return claim.store.readArtifact(ctx, claim.release, name)
}

// Executable returns only a verified executable in the immutable leased tree.
// The lease must remain held through creation and the execution's protection.
func (l *Lease) Executable(ctx context.Context, name string) (string, error) {
	if l == nil || l.claim == nil {
		return "", frame.ErrInvalid
	}
	claim := l.claim
	claim.mu.Lock()
	defer claim.mu.Unlock()
	if claim.store == nil {
		return "", frame.ErrClosed
	}
	var executable bool
	for _, artifact := range claim.release.Manifest.Artifacts {
		if artifact.Path == name {
			executable = artifact.Executable
			break
		}
	}
	if !executable {
		return "", ErrInvalid
	}
	if _, err := claim.store.readArtifact(ctx, claim.release, name); err != nil {
		return "", err
	}
	return filepath.Join(claim.store.directory, claim.release.Digest.String(), filepath.FromSlash(name)), nil
}

func (l *Lease) Close() error {
	if l == nil || l.claim == nil {
		return nil
	}
	claim := l.claim
	claim.mu.Lock()
	defer claim.mu.Unlock()
	store := claim.store
	if store == nil {
		return nil
	}
	claim.store = nil
	store.mu.Lock()
	defer store.mu.Unlock()
	digest := claim.release.Digest
	store.leases[digest]--
	if store.leases[digest] == 0 {
		delete(store.leases, digest)
	}
	store.notifyLocked()
	return nil
}

// Snapshot is a detached immutable declaration projection. Retaining it does not
// retain content; the original Lease owns that claim until Close.
func (l *Lease) Snapshot() Release {
	if l == nil || l.claim == nil {
		return Release{}
	}
	return l.claim.release.snapshot()
}

// Directory permits a trusted profile adapter to open the exact admitted tree.
// Keep this lease until every object that may use the directory has exited.
// Directory is not a writable data root or proof that a process has exited.
func (l *Lease) Directory() (string, error) {
	if l == nil || l.claim == nil {
		return "", frame.ErrInvalid
	}
	claim := l.claim
	claim.mu.Lock()
	defer claim.mu.Unlock()
	if claim.store == nil {
		return "", frame.ErrClosed
	}
	return claim.store.Path(claim.release.Digest)
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
	if err := found.verify(content, sealedTree); err != nil {
		return nil, err
	}
	return content.data, nil
}
