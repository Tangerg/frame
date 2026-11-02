//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package localfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFIFORefusedWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := OpenRegular(path)
		if file != nil {
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, fs.ErrInvalid) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		if writer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			writer.Close()
		}
		t.Fatal("open waited for a FIFO writer")
	}
}
