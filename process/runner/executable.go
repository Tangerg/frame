package runner

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Tangerg/frame/internal/localfile"
	framevalue "github.com/Tangerg/frame/value"
)

const (
	executableSnapshotMode   os.FileMode = 0500
	executableSnapshotPrefix             = "executable-"
)

func (s *Server) snapshotExecutable(request LaunchRequest) (_ string, err error) {
	input, err := localfile.OpenRegular(request.Command)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > s.config.MaxExecutableBytes {
		return "", errors.New("runner: invalid executable")
	}
	name := executableSnapshotPrefix + rand.Text()
	file, err := s.directory.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, localfile.PrivateFileMode)
	if err != nil {
		return "", err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			err = errors.Join(err, s.releaseExecutable(name))
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(input, s.config.MaxExecutableBytes+1))
	if err != nil {
		return "", err
	}
	if n > s.config.MaxExecutableBytes || framevalue.DigestOf([sha256.Size]byte(hash.Sum(nil))) != request.ExecutableDigest {
		return "", errors.New("runner: executable digest mismatch")
	}
	if err := file.Chmod(executableSnapshotMode); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Server) releaseExecutable(path string) error {
	if err := s.directory.Remove(path); err != nil {
		return fmt.Errorf("runner: executable snapshot cleanup failed: %w", err)
	}
	return nil
}
