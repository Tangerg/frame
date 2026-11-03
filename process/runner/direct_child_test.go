//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

const stderrDescendantMode = "FRAME_STDERR_DESCENDANT"

func TestDirectChildExitDoesNotWaitForDescendantStderr(t *testing.T) {
	server, _, ctx := serveFixture(t)
	checkDirectChildExit(t, server, ctx)
}

func TestDirectChildWaitWithNoControlTransport(t *testing.T) {
	server, err := Open(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	checkDirectChildExit(t, server, ctx)
}

func checkDirectChildExit(t *testing.T, server *Server, ctx context.Context) {
	t.Helper()
	if err := server.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	request := launchFixture(t, server, "stderr-descendant", 1, directory)
	request.Args = []string{"-test.run=^TestStderrDescendantHelper$"}
	request.Env = append(os.Environ(), stderrDescendantMode+"=parent")
	release := filepath.Join(directory, "release")
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0600)
		if data, err := os.ReadFile(filepath.Join(directory, "descendant")); err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	if _, err := server.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		observation, err := server.Inspect(deadline, request.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		if observation.State == Exited {
			if observation.ProofID == "" {
				t.Fatal("direct child exit lost its proof")
			}
			break
		}
		select {
		case <-deadline.Done():
			t.Fatal("descendant stderr retained the direct child's exit proof")
		case <-ticker.C:
		}
	}
	data, err := os.ReadFile(filepath.Join(directory, "descendant"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("descendant exited before direct-child proof: %v", err)
	}
}

func TestStderrDescendantHelper(t *testing.T) {
	switch os.Getenv(stderrDescendantMode) {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestStderrDescendantHelper$")
		child.Env = append(os.Environ(), stderrDescendantMode+"=descendant")
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("descendant", []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatal(err)
		}
	case "descendant":
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat("release"); !errors.Is(err, os.ErrNotExist) {
				return
			}
			select {
			case <-deadline.C:
				return
			case <-ticker.C:
			}
		}
	}
}
