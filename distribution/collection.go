package distribution

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func (s *storeAuthority) Remove(ctx context.Context, digest value.Digest) (err error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return err
	}
	defer done()
	if digest.Validate() != nil {
		return ErrInvalid
	}
	retiring, temporary, identity, err := s.privateDirectory(retiringDirectoryPrefix)
	if err != nil {
		return err
	}
	// Physical deletion follows withdrawal from the public namespace. A later
	// publication of the same digest cannot be mistaken for this old tree.
	defer func() { err = errors.Join(err, temporary.Close(), s.removePrivate(retiring, identity)) }()
	return s.admission.Under(ctx, func() error {
		if err := s.ensureUnused(ctx, digest); err != nil {
			return err
		}
		if err := frame.ContextError(ctx); err != nil {
			return err
		}
		if err := s.admission.RecoveryError(); err != nil {
			return err
		}
		withdrawn, err := s.withdraw(digest, retiring, identity)
		if withdrawn != nil {
			identity = withdrawn
		}
		return err
	})
}

func (s *storeAuthority) ensureUnused(ctx context.Context, digest value.Digest) error {
	s.mu.Lock()
	leased := s.leases[digest] != 0
	s.mu.Unlock()
	if leased {
		return framehost.ErrInUse
	}
	retained, err := s.retained(ctx, digest)
	if err != nil {
		return err
	}
	if retained {
		return framehost.ErrInUse
	}
	return nil
}

// withdraw renames a public release over the private retiring placeholder. It
// returns the identity now held at retiring once the rename has happened, even
// when the following durability sync fails.
func (s *storeAuthority) withdraw(digest value.Digest, retiring string, placeholder fs.FileInfo) (withdrawn fs.FileInfo, err error) {
	name := digest.String()
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, ErrIntegrity
	}
	releaseRoot, err := s.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, releaseRoot.Close()) }()
	opened, err := releaseRoot.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(ErrIntegrity, err)
	}
	current, err := s.root.Lstat(retiring)
	if err != nil || !os.SameFile(placeholder, current) {
		return nil, errors.Join(ErrIntegrity, err)
	}
	if err := s.root.Remove(retiring); err != nil {
		return nil, err
	}
	// Older macOS filesystems require the renamed directory to be writable.
	// Admission has excluded leases and retention; its contents stay sealed.
	if err := releaseRoot.Chmod(".", privateDirectoryMode); err != nil {
		return nil, err
	}
	if err := s.root.Rename(name, retiring); err != nil {
		restoreErr := releaseRoot.Chmod(".", info.Mode().Perm())
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return nil, errors.Join(err, restoreErr)
	}
	return info, s.lock.Sync()
}

// Digests observes the content namespace. Each later Remove still checks the
// original retention facts and live claims under the shared admission gate.
func (s *storeAuthority) Digests(ctx context.Context) (_ []value.Digest, err error) {
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
		if err := frame.ContextError(ctx); err != nil {
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
