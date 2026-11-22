package distribution

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/value"
)

// ImportDirectory publishes a release from a source directory and returns the
// lease that holds it; the caller closes that lease.
func (s *storeAuthority) ImportDirectory(ctx context.Context, directory string) (lease Lease, err error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return Lease{}, err
	}
	defer done()
	input, err := os.OpenRoot(directory)
	if err != nil {
		return Lease{}, err
	}
	defer func() {
		err = errors.Join(err, input.Close())
		if err != nil && lease.Valid() {
			err = errors.Join(err, lease.Close())
			lease = Lease{}
		}
	}()
	staged, err := s.stage()
	if err != nil {
		return Lease{}, err
	}
	defer func() { err = errors.Join(err, staged.discard()) }()
	err = staged.paths.walk(ctx, input, staged.limits.MaxEntries, func(name string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		return staged.copyRegular(ctx, input, name, entry)
	})
	if err != nil {
		return Lease{}, err
	}
	return s.publishHeld(ctx, staged, Source{Location: directory})
}

// Networking and trust policy belong to the application. This operation keeps
// the digest-pinned archive and its installed content under one original lease.
// ImportArchive publishes the archive whose digest is expected and returns the
// lease that holds it; the caller closes that lease.
func (s *storeAuthority) ImportArchive(ctx context.Context, fileName string, expected value.Digest) (lease Lease, err error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return Lease{}, err
	}
	defer done()
	if expected.Validate() != nil {
		return Lease{}, ErrInvalid
	}
	file, err := localfile.OpenRegular(fileName)
	if err != nil {
		return Lease{}, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil && lease.Valid() {
			err = errors.Join(err, lease.Close())
			lease = Lease{}
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return Lease{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > s.limits.MaxArchiveBytes {
		return Lease{}, ErrCapacity
	}
	data, err := readBounded(ctx, file, s.limits.MaxArchiveBytes)
	if err != nil {
		return Lease{}, err
	}
	if value.Hash(data) != expected {
		return Lease{}, ErrIntegrity
	}
	archive, err := s.limits.openArchive(data)
	if err != nil {
		return Lease{}, err
	}
	staged, err := s.stage()
	if err != nil {
		return Lease{}, err
	}
	defer func() { err = errors.Join(err, staged.discard()) }()
	if err := staged.extract(ctx, archive, s.limits); err != nil {
		return Lease{}, err
	}
	return s.publishHeld(ctx, staged, Source{Location: fileName, ArchiveDigest: expected})
}
