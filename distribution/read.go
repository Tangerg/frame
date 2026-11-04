package distribution

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/Tangerg/frame/internal/localfile"
)

// Bytes and permissions come from the same original file; separate reads could
// validate an executable flag against different content after a replacement.
type fileSnapshot struct {
	data []byte
	mode fs.FileMode
}

func readRegular(ctx context.Context, root *os.Root, name string, limit int64) (_ fileSnapshot, err error) {
	if !validPath(name) {
		return fileSnapshot{}, ErrInvalid
	}
	info, err := root.Lstat(name)
	if err != nil {
		return fileSnapshot{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return fileSnapshot{}, ErrIntegrity
	}
	if info.Mode()&specialFileModes != 0 {
		return fileSnapshot{}, ErrInvalid
	}
	file, err := localfile.OpenRegularIn(root, name)
	if err != nil {
		return fileSnapshot{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return fileSnapshot{}, err
	}
	if !os.SameFile(info, opened) || opened.Mode() != info.Mode() {
		return fileSnapshot{}, ErrIntegrity
	}
	data, err := readBounded(ctx, file, limit)
	if err != nil {
		return fileSnapshot{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return fileSnapshot{}, err
	}
	if opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || opened.Mode() != after.Mode() {
		return fileSnapshot{}, ErrIntegrity
	}
	return fileSnapshot{data: data, mode: opened.Mode()}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
func readBounded(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, ErrCapacity
	}
	reader = contextReader{ctx, reader}
	data, err := io.ReadAll(io.LimitReader(reader, limit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) < limit {
		return data, nil
	}
	// Probe separately so the full positive int64 range cannot overflow into
	// a negative LimitedReader bound and silently discard the original bytes.
	var extra [1]byte
	if n, err := io.ReadFull(reader, extra[:]); n != 0 {
		return nil, ErrCapacity
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data, nil
}
