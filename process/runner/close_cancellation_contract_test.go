package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestCloseAttemptPreservesWaitCancellationAndCompletedExit(t *testing.T) {
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
	if err := server.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, server, "cancel-wait", 1, t.TempDir())
	for i, entry := range request.Env {
		if strings.HasPrefix(entry, "FRAME_RUNNER_HELPER=") {
			request.Env[i] = "FRAME_RUNNER_HELPER=wait-for-kill"
		}
	}
	if _, err := server.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	live := server.processes[request.AttemptID]
	originalKill := live.kill
	server.mu.Unlock()
	defer func() { _ = originalKill() }()
	if ready, err := bufio.NewReader(live.stdout).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("original child readiness: %q %v", ready, err)
	}
	// Keep the original Wait pending after accepting termination, independently of scheduling.
	server.mu.Lock()
	live.kill = func() error { return nil }
	server.mu.Unlock()
	cause := errors.New("caller stopped waiting for original child")
	canceled, stop := context.WithCancelCause(ctx)
	stop(cause)
	expired, stopDeadline := context.WithTimeoutCause(ctx, 0, cause)
	defer stopDeadline()
	waits := []struct {
		ctx      context.Context
		category error
	}{{canceled, context.Canceled}, {expired, context.DeadlineExceeded}}
	for _, wait := range waits {
		t.Run(wait.category.Error(), func(t *testing.T) {
			observation, err := server.CloseAttempt(wait.ctx, request.AttemptRef)
			if !errors.Is(err, wait.category) || !errors.Is(err, cause) || observation.State != Running {
				t.Fatalf("pending original Wait lost cancellation: %+v, %v", observation, err)
			}
			if _, err := server.Launch(wait.ctx, request); !errors.Is(err, wait.category) || !errors.Is(err, cause) {
				t.Fatalf("pre-dispatch launch lost cancellation: %v", err)
			}
		})
	}
	if err := originalKill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live.done:
	case <-ctx.Done():
		t.Fatal("original child Wait did not finish")
	}
	proofID := ""
	for _, wait := range waits {
		observation, err := server.CloseAttempt(wait.ctx, request.AttemptRef)
		if err != nil || observation.State != Exited || observation.ProofID == "" {
			t.Fatalf("completed Wait lost its terminal proof to cancellation: %+v, %v", observation, err)
		}
		if proofID != "" && observation.ProofID != proofID {
			t.Fatalf("repeated close changed original exit proof: %q != %q", observation.ProofID, proofID)
		}
		proofID = observation.ProofID
	}
}

type cancellationControlConnection struct {
	net.Conn
	onWrite func()
	onRead  func()
}

func (c cancellationControlConnection) Write(data []byte) (int, error) {
	if c.onWrite != nil {
		c.onWrite()
	}
	return c.Conn.Write(data)
}

func (c cancellationControlConnection) Read(data []byte) (int, error) {
	if c.onRead != nil {
		c.onRead()
	}
	return c.Conn.Read(data)
}

func TestControlExchangePreservesCancellationAtIOBoundaries(t *testing.T) {
	encoded, err := encodeRequest(controlRequest{Operation: methodIdentity})
	if err != nil {
		t.Fatal(err)
	}
	for _, category := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, stage := range []string{"before-write", "write", "read"} {
			t.Run(category.Error()+"/"+stage, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					cause := errors.New("caller stopped waiting for runner control")
					var ctx context.Context
					var trigger, cleanup func()
					if category == context.Canceled {
						var cancel context.CancelCauseFunc
						ctx, cancel = context.WithCancelCause(t.Context())
						trigger = func() { cancel(cause) }
						cleanup = func() { cancel(nil) }
					} else {
						duration := time.Second
						if stage == "before-write" {
							duration = 0
						}
						ctx, cleanup = context.WithTimeoutCause(t.Context(), duration, cause)
						trigger = func() {}
					}
					defer cleanup()
					client, peer := net.Pipe()
					defer client.Close()
					defer peer.Close()
					connection := cancellationControlConnection{Conn: client}
					var peerRead chan error
					switch stage {
					case "before-write":
						trigger()
					case "write":
						connection.onWrite = trigger
					case "read":
						connection.onRead = trigger
						peerRead = make(chan error, 1)
						go func() {
							_, err := bufio.NewReader(peer).ReadString('\n')
							peerRead <- err
						}()
					}
					_, dispatch, err := exchangeControl(ctx, connection, methodIdentity, encoded)
					if !errors.Is(err, category) || !errors.Is(err, cause) {
						t.Fatalf("control exchange lost cancellation category or cause: %v", err)
					}
					if stage == "before-write" {
						if dispatch != controlNotDispatched {
							t.Fatalf("pre-write cancellation reported dispatch %v", dispatch)
						}
					} else if dispatch != controlDispatchUnknown || !errors.Is(err, io.ErrClosedPipe) {
						t.Fatalf("I/O cancellation lost uncertain dispatch or original I/O error: %v, %v", dispatch, err)
					}
					if peerRead != nil {
						if err := <-peerRead; err != nil {
							t.Fatalf("read cancellation happened before request delivery: %v", err)
						}
					}
				})
			})
		}
	}
}
