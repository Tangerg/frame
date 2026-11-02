package localfile

import (
	"errors"
	"io/fs"
	"os"
)

func OpenRegular(path string) (*os.File, error) {
	return regular(os.OpenFile(path, os.O_RDONLY|nonblocking, 0))
}

func OpenRegularIn(root *os.Root, path string) (*os.File, error) {
	return regular(root.OpenFile(path, os.O_RDONLY|nonblocking, 0))
}

func regular(f *os.File, err error) (*os.File, error) {
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &fs.PathError{Op: "open regular file", Path: f.Name(), Err: fs.ErrInvalid}
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
