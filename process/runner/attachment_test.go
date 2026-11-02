package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countedPipe struct {
	*os.File
	closes atomic.Int32
}

func (p *countedPipe) Close() error {
	p.closes.Add(1)
	return p.File.Close()
}

func trackStdout(t *testing.T, server *Server, id string) *countedPipe {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	live := server.processes[id]
	if live == nil || live.stdout == nil {
		t.Fatal("child stdout is unavailable before attachment")
	}
	pipe := &countedPipe{File: live.stdout.(*os.File)}
	live.stdout = pipe
	return pipe
}

func assertPipeReleased(t *testing.T, pipe *countedPipe) {
	t.Helper()
	if _, err := pipe.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("original stdout remains open: %v", err)
	}
	if count := pipe.closes.Load(); count != 1 {
		t.Fatalf("stdout had competing release owners: closes=%d", count)
	}
}

type failedAttachConnection struct {
	*bytes.Reader
	writeErr    error
	deadlineErr error
}

func (c failedAttachConnection) Write(data []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(data), nil
}
func (failedAttachConnection) Close() error                     { return nil }
func (failedAttachConnection) LocalAddr() net.Addr              { return &net.UnixAddr{Net: "unix"} }
func (failedAttachConnection) RemoteAddr() net.Addr             { return &net.UnixAddr{Net: "unix"} }
func (c failedAttachConnection) SetDeadline(time.Time) error    { return c.deadlineErr }
func (failedAttachConnection) SetReadDeadline(time.Time) error  { return nil }
func (failedAttachConnection) SetWriteDeadline(time.Time) error { return nil }

func TestFailedAttachmentReleasesOriginalStdout(t *testing.T) {
	failure := errors.New("attachment peer disconnected")
	for _, test := range []struct {
		name        string
		writeErr    error
		deadlineErr error
	}{
		{name: "acknowledgement", writeErr: failure},
		{name: "deadline", deadlineErr: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := Open(Config{Directory: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := server.Fence(ctx, 1); err != nil {
				t.Fatal(err)
			}
			request := launchFixture(t, server, "failed-attachment", 1, t.TempDir())
			if _, err := server.Launch(ctx, request); err != nil {
				t.Fatal(err)
			}
			pipe := trackStdout(t, server, request.AttemptID)
			encoded, err := json.Marshal(controlRequest{Operation: methodAttach, Attempt: &request.AttemptRef})
			if err != nil {
				t.Fatal(err)
			}
			connection := failedAttachConnection{bytes.NewReader(append(encoded, '\n')), test.writeErr, test.deadlineErr}
			server.handle(ctx, connection, func() { t.Error("failed attachment was admitted for streaming") })
			if exit, err := server.Inspect(ctx, request.AttemptID); err != nil || exit.State != Exited {
				t.Fatalf("original child exit: %+v %v", exit, err)
			}
			assertPipeReleased(t, pipe)
		})
	}
}

func TestAttachmentReleaseJoinsOriginalChildAndPipe(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "child-exit"
		if concurrent {
			name = "concurrent-close"
		}
		t.Run(name, func(t *testing.T) {
			server, client, ctx := serveFixture(t)
			if err := client.Fence(ctx, 1); err != nil {
				t.Fatal(err)
			}
			request := launchFixture(t, client, "attached-pipe", 1, t.TempDir())
			if _, err := client.Launch(ctx, request); err != nil {
				t.Fatal(err)
			}
			pipe := trackStdout(t, server, request.AttemptID)
			stream, err := client.Attach(ctx, request.AttemptRef)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stream.Close() })
			if _, err := io.WriteString(stream, "ready\n"); err != nil {
				t.Fatal(err)
			}
			if line, err := bufio.NewReader(stream).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("attached child readiness: %q %v", line, err)
			}
			if concurrent {
				var closing sync.WaitGroup
				closing.Go(func() { _ = stream.Close() })
				closing.Go(func() {
					if _, err := server.CloseAttempt(ctx, request.AttemptRef); err != nil {
						t.Error(err)
					}
				})
				closing.Go(func() {
					if err := server.Close(); err != nil {
						t.Error(err)
					}
				})
				closing.Wait()
			} else {
				if _, err := client.CloseAttempt(ctx, request.AttemptRef); err != nil {
					t.Fatal(err)
				}
				if output, err := io.ReadAll(stream); err != nil || len(output) != 0 {
					t.Fatalf("original output stream did not reach EOF: %q %v", output, err)
				}
				if err := server.Close(); err != nil {
					t.Fatal(err)
				}
			}
			assertPipeReleased(t, pipe)
		})
	}
}
