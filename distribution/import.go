package distribution

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/value"
)

func (s *Store) ImportDirectoryHeld(ctx context.Context, directory string) (lease *Lease, err error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return nil, err
	}
	defer done()
	input, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, input.Close())
		if err != nil && lease != nil {
			err = errors.Join(err, lease.Close())
			lease = nil
		}
	}()
	staged, err := s.stage()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, staged.discard()) }()
	err = staged.paths.walk(ctx, input, staged.limits.Entries, func(name string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		return staged.copyRegular(ctx, input, name, entry)
	})
	if err != nil {
		return nil, err
	}
	return s.publishHeld(ctx, staged, Source{Location: directory})
}

// Networking and trust policy belong to the application. This operation keeps
// the digest-pinned archive and its installed content under one original lease.
func (s *Store) ImportArchiveHeld(ctx context.Context, fileName string, expected value.Digest) (lease *Lease, err error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return nil, err
	}
	defer done()
	if expected.Validate() != nil {
		return nil, ErrInvalid
	}
	file, err := localfile.OpenRegular(fileName)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil && lease != nil {
			err = errors.Join(err, lease.Close())
			lease = nil
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > s.limits.ArchiveBytes {
		return nil, ErrCapacity
	}
	data, err := readBounded(ctx, file, s.limits.ArchiveBytes)
	if err != nil {
		return nil, err
	}
	if value.Hash(data) != expected {
		return nil, ErrIntegrity
	}
	archive, err := s.limits.openArchive(data)
	if err != nil {
		return nil, err
	}
	staged, err := s.stage()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, staged.discard()) }()
	if err := staged.extract(ctx, archive, s.limits); err != nil {
		return nil, err
	}
	return s.publishHeld(ctx, staged, Source{Location: fileName, ArchiveDigest: expected})
}

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
