//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDaemonCloseDistinguishesItsStopFromAnOriginalCrash(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "requested stop"
		if external {
			name = "prior interrupt"
		}
		t.Run(name, func(t *testing.T) {
			directory, err := os.MkdirTemp("", "frame-daemon-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			executable := filepath.Join(directory, "unready")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0700); err != nil {
				t.Fatal(err)
			}
			original, err := NewDaemon(DaemonConfig{Executable: executable, Directory: directory, StartTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = original.Close(ctx)
			})
			if _, err := original.Identity(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unready startup: %v", err)
			}
			if original.command == nil {
				t.Fatal("startup did not create the original process")
			}
			if external {
				if err := original.command.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
				<-original.exited
			}
			err = original.Close(t.Context())
			if external {
				if _, ok := errors.AsType[*exec.ExitError](err); !ok {
					t.Fatalf("original crash was hidden: %v", err)
				}
			} else if err != nil {
				t.Fatalf("requested stop did not confirm the original exit: %v", err)
			}
		})
	}
}
