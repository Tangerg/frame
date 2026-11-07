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
	"testing"
	"time"

	"github.com/Tangerg/frame"
)

func attachmentFixture(t *testing.T, helper string) (*Server, LaunchRequest, *process, *countedPipe, context.Context, context.CancelFunc) {
	t.Helper()
	server, err := Open(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(func() {
		cancel()
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := server.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, server, "attachment", 1, t.TempDir())
	request.Env = []string{"FRAME_RUNNER_HELPER=" + helper}
	if _, err := server.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	pipe := trackStdout(t, server, request.AttemptID)
	server.mu.Lock()
	live := server.processes[request.AttemptID]
	server.mu.Unlock()
	return server, request, live, pipe, ctx, cancel
}

func openAttachmentPipe(t *testing.T, server *Server, ctx context.Context, method controlMethod, ref AttemptRef) (net.Conn, *bufio.Reader, <-chan struct{}, error) {
	t.Helper()
	host, runner := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer runner.Close()
		server.handle(ctx, runner, func() {})
	}()
	t.Cleanup(func() {
		_ = host.Close()
		joined, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		waitAttachment(t, joined, done)
	})
	if deadline, ok := ctx.Deadline(); ok {
		if err := host.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRequest(host, controlRequest{Operation: method, Attempt: &ref}); err != nil {
		return host, nil, done, err
	}
	reader := bufio.NewReader(host)
	line, err := readLine(reader)
	if err == nil {
		_, err = decodeResponse(line, method)
	}
	return host, reader, done, err
}

func waitAttachment(t *testing.T, ctx context.Context, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("attachment did not join its original workers")
	}
}

type observedInput struct {
	io.WriteCloser
	once    sync.Once
	entered chan struct{}
}

func (w *observedInput) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	return w.WriteCloser.Write(data)
}

func fillChildInput(t *testing.T, input *os.File) {
	t.Helper()
	data := bytes.Repeat([]byte("x"), 8<<20)
	// A timed-out write establishes real OS-pipe backpressure before the test
	// disconnects anything; production attachment shutdown has no write timeout.
	if err := input.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	n, err := input.Write(data)
	if !errors.Is(err, os.ErrDeadlineExceeded) || n == 0 || n == len(data) {
		t.Fatalf("child stdin did not become backpressured: bytes=%d error=%v", n, err)
	}
	if err := input.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentControlClosesBackpressuredOriginalChild(t *testing.T) {
	for _, interrupt := range []string{"host-disconnect", "client-close", "server-cancel"} {
		t.Run(interrupt, func(t *testing.T) {
			server, request, live, pipe, ctx, cancel := attachmentFixture(t, "wait-for-kill")
			fillChildInput(t, live.stdin.(*os.File))
			input := &observedInput{WriteCloser: live.stdin, entered: make(chan struct{})}
			server.mu.Lock()
			live.stdin = input
			server.mu.Unlock()
			control, _, controlDone, err := openAttachmentPipe(t, server, ctx, methodAttachControl, request.AttemptRef)
			if err != nil {
				t.Fatal(err)
			}
			stream, reader, streamDone, err := openAttachmentPipe(t, server, ctx, methodAttachStream, request.AttemptRef)
			if err != nil {
				t.Fatal(err)
			}
			if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("child readiness: %q %v", line, err)
			}
			written := make(chan struct{})
			go func() {
				defer close(written)
				_, _ = stream.Write([]byte("blocked input"))
			}()
			waitAttachment(t, ctx, input.entered)
			switch interrupt {
			case "host-disconnect":
				_ = control.Close()
			case "client-close":
				client := &clientAttachment{stream: stream, reader: reader, control: control}
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
			case "server-cancel":
				cancel()
			}
			joined, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			waitAttachment(t, joined, controlDone)
			waitAttachment(t, joined, streamDone)
			waitAttachment(t, joined, written)
			waitAttachment(t, joined, live.done)
			observation, err := server.Inspect(joined, request.AttemptID)
			if err != nil || observation.State != Exited || observation.ProofID == "" || observation.ExitCode == nil {
				t.Fatalf("missing original Wait evidence: %+v %v", observation, err)
			}
			assertPipeReleased(t, pipe)
		})
	}
}

func TestFailedAttachmentStreamReleasesControlAndOriginalChild(t *testing.T) {
	failure := errors.New("attachment stream handshake failed")
	for _, test := range []struct {
		name        string
		writeErr    error
		deadlineErr error
	}{
		{name: "acknowledgement", writeErr: failure},
		{name: "deadline", deadlineErr: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, request, _, pipe, ctx, _ := attachmentFixture(t, "wait-for-kill")
			_, _, controlDone, err := openAttachmentPipe(t, server, ctx, methodAttachControl, request.AttemptRef)
			if err != nil {
				t.Fatal(err)
			}
			requestJSON, err := json.Marshal(controlRequest{Operation: methodAttachStream, Attempt: &request.AttemptRef})
			if err != nil {
				t.Fatal(err)
			}
			connection := failedAttachConnection{bytes.NewReader(append(requestJSON, '\n')), test.writeErr, test.deadlineErr}
			server.handle(ctx, connection, func() { t.Error("failed stream was admitted for forwarding") })
			waitAttachment(t, ctx, controlDone)
			observation, err := server.Inspect(ctx, request.AttemptID)
			if err != nil || observation.State != Exited || observation.ProofID == "" {
				t.Fatalf("failed stream stranded original child: %+v %v", observation, err)
			}
			assertPipeReleased(t, pipe)
		})
	}
}

func TestAttachmentControlRetiresBeforeStreamConnects(t *testing.T) {
	for _, interrupt := range []string{"host-disconnect", "server-cancel", "child-exit"} {
		t.Run(interrupt, func(t *testing.T) {
			server, request, live, pipe, ctx, cancel := attachmentFixture(t, "wait-for-kill")
			control, _, done, err := openAttachmentPipe(t, server, ctx, methodAttachControl, request.AttemptRef)
			if err != nil {
				t.Fatal(err)
			}
			switch interrupt {
			case "host-disconnect":
				_ = control.Close()
			case "server-cancel":
				cancel()
			case "child-exit":
				if _, err := server.CloseAttempt(ctx, request.AttemptRef); err != nil {
					t.Fatal(err)
				}
			}
			joined, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			waitAttachment(t, joined, done)
			waitAttachment(t, joined, live.done)
			if _, _, _, err := openAttachmentPipe(t, server, joined, methodAttachStream, request.AttemptRef); !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("late stream revived a closed attachment: %v", err)
			}
			assertPipeReleased(t, pipe)
		})
	}
}

func TestAttachmentRequiresOriginalControlAndOneStream(t *testing.T) {
	server, request, _, pipe, ctx, _ := attachmentFixture(t, "wait-for-kill")
	if _, _, _, err := openAttachmentPipe(t, server, ctx, methodAttachStream, request.AttemptRef); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("data connection acquired attachment ownership: %v", err)
	}
	control, _, controlDone, err := openAttachmentPipe(t, server, ctx, methodAttachControl, request.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	foreign := request.AttemptRef
	foreign.Epoch++
	if _, _, _, err := openAttachmentPipe(t, server, ctx, methodAttachStream, foreign); !errors.Is(err, ErrFenced) {
		t.Fatalf("foreign stream joined original attachment: %v", err)
	}
	_, reader, streamDone, err := openAttachmentPipe(t, server, ctx, methodAttachStream, request.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := openAttachmentPipe(t, server, ctx, methodAttachStream, request.AttemptRef); !errors.Is(err, ErrAttached) {
		t.Fatalf("original attachment admitted a second stream: %v", err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("refusals damaged original stream: %q %v", line, err)
	}
	_ = control.Close()
	waitAttachment(t, ctx, controlDone)
	waitAttachment(t, ctx, streamDone)
	assertPipeReleased(t, pipe)
}

func TestAttachmentDrainsFinalOutputAfterOriginalWait(t *testing.T) {
	server, request, live, pipe, ctx, _ := attachmentFixture(t, "reply-and-exit")
	_, _, controlDone, err := openAttachmentPipe(t, server, ctx, methodAttachControl, request.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	stream, reader, streamDone, err := openAttachmentPipe(t, server, ctx, methodAttachStream, request.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	waitAttachment(t, ctx, live.done)
	output, err := io.ReadAll(reader)
	if err != nil || string(output) != "final\n" {
		t.Fatalf("original Wait discarded final stdout: %q %v", output, err)
	}
	waitAttachment(t, ctx, controlDone)
	waitAttachment(t, ctx, streamDone)
	assertPipeReleased(t, pipe)
}
