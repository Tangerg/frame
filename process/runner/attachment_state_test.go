package runner

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
)

func TestAttachmentAfterOriginalExitIsUnavailable(t *testing.T) {
	server, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "original-exit", 1, t.TempDir())
	if _, err := client.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	exit, err := client.CloseAttempt(ctx, request.AttemptRef)
	if err != nil || exit.State != Exited || exit.ProofID == "" {
		t.Fatalf("original child exit: %+v %v", exit, err)
	}
	if _, err := client.Attach(ctx, request.AttemptRef); !errors.Is(err, ErrUnavailable) || errors.Is(err, frame.ErrClosed) {
		t.Errorf("confirmed child exit became runner shutdown: %v", err)
	}
	for _, acknowledged := range []bool{false, true} {
		if acknowledged {
			receipt, err := exit.ExitReceipt()
			if err != nil {
				t.Fatal(err)
			}
			if err := client.AcknowledgeExit(ctx, receipt); err != nil {
				t.Fatal(err)
			}
		}
		for _, method := range []controlMethod{methodAttachControl, methodAttachStream} {
			if _, _, _, err := openAttachmentPipe(t, server, ctx, method, request.AttemptRef); !errors.Is(err, ErrUnavailable) {
				t.Errorf("%s lost confirmed original exit: %v", method, err)
			}
			for _, foreign := range []AttemptRef{
				{AttemptID: request.AttemptID, Epoch: request.Epoch + 1, RunnerID: request.RunnerID},
				{AttemptID: request.AttemptID, Epoch: request.Epoch, RunnerID: "another-runner"},
			} {
				if _, _, _, err := openAttachmentPipe(t, server, ctx, method, foreign); !errors.Is(err, ErrFenced) {
					t.Errorf("%s accepted another creation domain: %v", method, err)
				}
			}
		}
	}
	if identity, err := client.Identity(ctx); err != nil || identity.ID != request.RunnerID {
		t.Fatalf("child exit closed its runner: %+v %v", identity, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	for _, method := range []controlMethod{methodAttachControl, methodAttachStream} {
		if _, _, _, err := openAttachmentPipe(t, server, ctx, method, request.AttemptRef); !errors.Is(err, frame.ErrClosed) {
			t.Errorf("%s lost original runner shutdown: %v", method, err)
		}
	}
}

func TestFailedLaunchAttachmentIsUnavailable(t *testing.T) {
	server, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "not-created", 1, t.TempDir())
	request.Directory = filepath.Join(t.TempDir(), "missing")
	result, err := client.Launch(ctx, request)
	if err == nil || result.State != NotCreated || result.ProofID == "" {
		t.Fatalf("failed launch lost original non-creation proof: %+v %v", result, err)
	}
	for _, method := range []controlMethod{methodAttachControl, methodAttachStream} {
		if _, _, _, err := openAttachmentPipe(t, server, ctx, method, request.AttemptRef); !errors.Is(err, ErrUnavailable) || errors.Is(err, frame.ErrClosed) {
			t.Errorf("%s confused failed creation with runner shutdown: %v", method, err)
		}
	}
	if _, err := client.Identity(ctx); err != nil {
		t.Fatalf("failed child launch closed original runner: %v", err)
	}
}
