//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
)

func TestInvalidSocketAddressNeverCreatesOrReconnectsAnAuthority(t *testing.T) {
	directory := filepath.Join(t.TempDir(), strings.Repeat("x", socketPathLimit()))
	socket := filepath.Join(directory, "control.sock")
	if _, err := NewDaemon(DaemonConfig{Executable: "/unstarted-runner", Directory: directory, StartTimeout: time.Second}); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("long daemon address: %v", err)
	}
	if _, err := Listen(socket); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("long listener address: %v", err)
	}
	if _, err := Dial(socket).Identity(t.Context()); !errors.Is(err, frame.ErrInvalid) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unusable address was treated as absent authority: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation created state: %v", err)
	}
}

func TestSocketParentUsesTheOriginalPhysicalDirectory(t *testing.T) {
	directory, err := os.MkdirTemp("", "frame-alias-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	owned := filepath.Join(directory, "owned")
	server, err := Open(Config{Directory: owned})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	alias := filepath.Join(directory, "alias")
	if err := os.Symlink(owned, alias); err != nil {
		t.Fatal(err)
	}
	listener, err := server.Listen(filepath.Join(alias, "socket"))
	if err != nil {
		t.Fatalf("physical directory alias was refused: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if listener, err := server.Listen(filepath.Join(directory, "foreign.sock")); err == nil {
		_ = listener.Close()
		t.Fatal("socket escaped its original directory")
	}
}

func TestRunnerRejectsAuthorityDirectoryWithForeignWriters(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(Config{Directory: dir}); err == nil {
		_ = s.Close()
		t.Fatal("accepted shared state directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "authority.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untrusted directory used: %v", err)
	}
}
