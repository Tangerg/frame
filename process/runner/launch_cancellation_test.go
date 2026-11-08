package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame/value"
)

func TestLaunchCancellationDuringSnapshotSettlesOriginalAttempt(t *testing.T) {
	ctx, timeout := context.WithTimeout(t.Context(), 10*time.Second)
	defer timeout()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	const preparationBytes = 64 << 20
	image = append(image, bytes.Repeat([]byte{0}, preparationBytes)...)
	directory := t.TempDir()
	program := filepath.Join(directory, "slow-snapshot")
	if err := os.WriteFile(program, image, 0700); err != nil {
		t.Fatal(err)
	}
	config := Config{Directory: filepath.Join(directory, "runner"), MaxExecutableBytes: int64(len(image))}
	server, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := server.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	identity, err := server.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := LaunchRequest{
		AttemptRef: AttemptRef{AttemptID: "original-cancelled-launch", Epoch: 1, RunnerID: identity.ID},
		Command:    program, Args: []string{"-test.run=^TestRunnerProcessHelper$"}, ExecutableDigest: value.Hash(image), Configuration: value.Hash(nil),
	}
	cause := errors.New("original launch cancelled while preparing its executable")
	call, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	finished := make(chan struct{})
	observed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			entries, err := os.ReadDir(config.Directory)
			if err != nil {
				observed <- err
				return
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "executable-") {
					cancel(cause)
					observed <- nil
					return
				}
			}
			select {
			case <-finished:
				observed <- errors.New("launch completed before its snapshot was observed")
				return
			case <-ctx.Done():
				observed <- ctx.Err()
				return
			case <-ticker.C:
			}
		}
	}()
	result, err := server.Launch(call, request)
	close(finished)
	if observationErr := <-observed; observationErr != nil {
		t.Fatal(observationErr)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || result.State != NotCreated || result.PID != 0 || result.ProofID == "" || result.NotDispatched(request) {
		t.Fatalf("cancelled preparation did not settle the original reservation: %+v %v", result, err)
	}
	entries, err := os.ReadDir(config.Directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "executable-") {
			t.Fatalf("cancelled preparation retained snapshot %q", entry.Name())
		}
	}
	if observed, err := server.Inspect(ctx, request.AttemptID); err != nil || observed.State != NotCreated || observed.ProofID != result.ProofID {
		t.Fatalf("original non-creation evidence changed: %+v %v", observed, err)
	}
	if replay, err := server.Launch(ctx, request); err != nil || replay.State != NotCreated || replay.ProofID != result.ProofID || replay.NotDispatched(request) {
		t.Fatalf("cancelled attempt was recreated: %+v %v", replay, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if replay, err := reopened.Launch(ctx, request); err != nil || replay.State != NotCreated || replay.ProofID != result.ProofID {
		t.Fatalf("restart lost the original cancellation receipt: %+v %v", replay, err)
	}
}
