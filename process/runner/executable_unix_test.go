//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestFIFOIsRejectedBeforeReading(t *testing.T) {
	s, c, ctx := serveFixture(t)
	if err := c.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	r := launchFixture(t, c, "fifo", 1, t.TempDir())
	r.Command = filepath.Join(r.Directory, "pipe")
	if err := syscall.Mkfifo(r.Command, 0600); err != nil {
		t.Fatal(err)
	}
	o, err := c.Launch(ctx, r)
	if err == nil || o.State != Exited {
		t.Fatalf("fifo: %+v %v", o, err)
	}
	if _, err := s.Identity(ctx); err != nil {
		t.Fatal(err)
	}
}
