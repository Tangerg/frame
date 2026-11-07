package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
)

func TestDaemonKeepsTheOriginalProcessAuthorityAndJoinsItsExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	executable := filepath.Join(t.TempDir(), "frame-runner")
	build := exec.CommandContext(ctx, "go", "build", "-o", executable, "../../cmd/frame-runner")
	build.Env = append(os.Environ(), "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, output)
	}
	directory, err := os.MkdirTemp("", "frame-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	config := DaemonConfig{Executable: executable, Directory: directory, StartTimeout: 5 * time.Second}
	original, err := NewDaemon(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		wait, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := original.Close(wait); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.Stat(filepath.Join(directory, "control.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("constructor created an authority: %v", err)
	}
	var calls sync.WaitGroup
	identities := make(chan Identity, 12)
	failures := make(chan error, 12)
	for range 12 {
		calls.Go(func() {
			identity, err := original.Identity(ctx)
			identities <- identity
			failures <- err
		})
	}
	calls.Wait()
	close(identities)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first Identity
	for identity := range identities {
		if first == (Identity{}) {
			first = identity
		}
		if identity != first {
			t.Fatal("concurrent callers created distinct authorities")
		}
	}
	attached, err := NewDaemon(config)
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := attached.Identity(ctx); err != nil || actual != first {
		t.Fatalf("reconnect replaced original authority: %+v %v", actual, err)
	}
	if err := attached.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := original.Identity(ctx); err != nil {
		t.Fatalf("non-owner stopped the runner: %v", err)
	}
	if _, err := attached.Identity(ctx); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("closed client reopened: %v", err)
	}
	if err := original.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, original, "daemon-original", 1, t.TempDir())
	if actual, err := original.Launch(ctx, request); err != nil || actual.State != Running {
		t.Fatalf("launch: %+v %v", actual, err)
	}
	if actual, err := original.CloseAttempt(ctx, request.AttemptRef); err != nil || actual.State != Exited || actual.ProofID == "" {
		t.Fatalf("original child exit: %+v %v", actual, err)
	}
	withdrawn, stop := context.WithCancel(ctx)
	stop()
	_ = original.Close(withdrawn)
	// Cancellation only abandons the caller's wait. The original close must
	// finish without requiring a second caller to drive process termination.
	select {
	case <-original.closeDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := original.Close(ctx); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(filepath.Join(directory, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Identity(ctx); err == nil {
		t.Fatal("original runner still served after its Wait completed")
	}
}
