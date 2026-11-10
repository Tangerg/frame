package localfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const generatedFilePattern = ".frame-generated-*"

// Readers and existing hard links must retain the previous file until all bytes
// are ready, so replacement happens only after the staged file is closed.
func WriteGenerated(path string, body []byte) (err error) {
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if info != nil && !info.Mode().IsRegular() {
		return &fs.PathError{Op: "write generated file", Path: path, Err: fs.ErrInvalid}
	}
	file, err := os.CreateTemp(filepath.Dir(path), generatedFilePattern)
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(file.Name()); !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Chmod(PublicFileMode); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
