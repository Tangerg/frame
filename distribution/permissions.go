package distribution

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame/internal/localfile"
)

const (
	privateDirectoryMode = localfile.PrivateDirectoryMode
	sealedDirectoryMode  = fs.FileMode(0o500)
	privateFileMode      = localfile.PrivateFileMode
	sealedFileMode       = fs.FileMode(0o400)
	ownerExecuteBit      = fs.FileMode(0o100)
	fileExecuteMask      = fs.FileMode(0o111)
	specialFileModes     = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
)

func sealContents(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		mode := sealedFileMode
		if entry.IsDir() {
			mode = sealedDirectoryMode
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode |= info.Mode().Perm() & fileExecuteMask
		}
		return root.Chmod(name, mode)
	})
}

func removeSealed(directory string) error {
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	err := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(name, privateDirectoryMode)
		}
		return nil
	})
	return errors.Join(err, os.RemoveAll(directory))
}
