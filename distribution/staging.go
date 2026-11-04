package distribution

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/Tangerg/frame/internal/localfile"
)

type staging struct {
	store    *Store
	name     string
	identity fs.FileInfo
	root     *os.Root
	limits   Limits
	paths    pathInventory
	bytes    int64
}

func (s *Store) stage() (*staging, error) {
	name, root, identity, err := s.privateDirectory(stagingDirectoryPrefix)
	if err != nil {
		return nil, err
	}
	return &staging{store: s, name: name, identity: identity, root: root, limits: s.limits, paths: pathInventory{}}, nil
}

func (s *staging) discard() error {
	return errors.Join(s.root.Close(), s.store.removePrivate(s.name, s.identity))
}

func (s *staging) admitPath(name string, kind entryKind) error {
	if err := s.paths.admit(name, kind); err != nil {
		return err
	}
	if len(s.paths) > s.limits.Entries {
		return ErrCapacity
	}
	return nil
}

func (s *staging) write(ctx context.Context, name string, mode fs.FileMode, reader io.Reader) error {
	if mode.Type() != 0 || mode&specialFileModes != 0 {
		return ErrInvalid
	}
	if err := s.admitPath(name, fileEntry); err != nil {
		return err
	}
	if err := s.root.MkdirAll(path.Dir(name), privateDirectoryMode); err != nil {
		return err
	}
	limit := s.limits.ExpandedBytes - s.bytes
	if isReleaseMetadata(name) {
		limit = min(limit, s.limits.ManifestBytes)
	}
	data, err := readBounded(ctx, reader, limit)
	if err != nil {
		return err
	}
	s.bytes += int64(len(data))
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode|mode.Perm()&fileExecuteMask)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

// Source paths can change during staging; the copied file must retain its
// original identity, size and modification time through the read.
func (s *staging) copyRegular(ctx context.Context, input *os.Root, name string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: only regular files are accepted", ErrInvalid)
	}
	file, err := localfile.OpenRegularIn(input, name)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		return errors.Join(err, file.Close())
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return errors.Join(ErrIntegrity, file.Close())
	}
	copyErr := s.write(ctx, name, opened.Mode(), file)
	after, statErr := file.Stat()
	if statErr == nil && (!os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime())) {
		statErr = ErrIntegrity
	}
	return errors.Join(copyErr, statErr, file.Close())
}

func (s *staging) extract(ctx context.Context, archive *zip.Reader, limits Limits) error {
	for _, entry := range archive.File {
		name := strings.TrimSuffix(entry.Name, "/")
		if entry.Mode().Type() != 0 && entry.Mode().Type() != fs.ModeDir {
			return ErrInvalid
		}
		if entry.Mode().IsDir() {
			if err := s.admitPath(name, directoryEntry); err != nil {
				return err
			}
			continue
		}
		if !limits.admitsExpansion(entry.UncompressedSize64, entry.CompressedSize64) {
			return ErrCapacity
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		if err := errors.Join(s.write(ctx, name, entry.Mode(), input), input.Close()); err != nil {
			return err
		}
	}
	return nil
}
