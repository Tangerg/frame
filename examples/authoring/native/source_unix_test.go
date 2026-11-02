//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package native

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/Tangerg/frame/examples/document"
	"github.com/Tangerg/frame/value"
)

func TestSourceRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	ctx := authorContext(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "source.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	result := make(chan error, 1)
	go func() {
		_, err := readSource(ctx, root, document.DocumentRef{Path: "source.fifo", Revision: value.Hash(nil)})
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("FIFO was not refused as a nonregular source: %v", err)
		}
	case <-ctx.Done():
		// An old blocking reader must be released before test cleanup.
		if descriptor, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(descriptor)
		}
		t.Fatal("source open waited for a FIFO writer")
	}
}
