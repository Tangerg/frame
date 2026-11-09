package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestExecutableLimitRejectsOverflowBeforeCreatingAuthority(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "authority")
	server, err := Open(Config{Directory: directory, MaxExecutableBytes: math.MaxInt64})
	if server != nil {
		_ = server.Close()
	}
	if !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("executable limit can overflow its bounded reader: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid capacity created an authority directory: %v", err)
	}
}

func TestLaunchRejectsUnrepresentableProgramBeforeReservation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct{ name, text string }{{"invalid-utf8", "\xff"}, {"NUL", "\x00"}} {
		for _, field := range []string{"argument", "environment", "command", "directory"} {
			t.Run(field+"/"+invalid.name, func(t *testing.T) {
				server, err := Open(Config{Directory: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = server.Close() })
				if err := server.Fence(t.Context(), 1); err != nil {
					t.Fatal(err)
				}
				request := LaunchRequest{
					AttemptRef: AttemptRef{AttemptID: "original", Epoch: 1, RunnerID: server.id},
					Command:    executable, ExecutableDigest: value.Hash(program), Configuration: value.Hash(nil),
					Args: []string{"-test.run=^TestRunnerProcessHelper$"},
				}
				switch field {
				case "argument":
					request.Args = append(request.Args, "--", invalid.text)
				case "environment":
					request.Env = []string{"PRIVATE_TOKEN=" + invalid.text}
				case "command":
					request.Command = filepath.Join(t.TempDir(), invalid.text)
				case "directory":
					request.Directory = filepath.Join(t.TempDir(), invalid.text)
				}
				observation, err := server.Launch(t.Context(), request)
				if !errors.Is(err, frame.ErrInvalid) || observation.AttemptID != "" {
					t.Fatalf("invalid program crossed launch admission: %+v, %v", observation, err)
				}
				if _, err := server.Inspect(t.Context(), request.AttemptID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("invalid program reserved a durable attempt: %v", err)
				}
			})
		}
	}
}

func TestControlRequestPreservesProgramTextOrRejectsBeforeWriting(t *testing.T) {
	for _, input := range []struct{ name, text string }{{"invalid-utf8", "\xff"}, {"replacement-rune", "replacement \ufffd"}, {"non-BMP", "unicode \U0001d11e"}} {
		t.Run(input.name, func(t *testing.T) {
			request := launchControlRequest{LaunchRequest{Args: []string{input.text}, ExecutableDigest: value.Hash(nil), Configuration: value.Hash(nil)}}
			var output bytes.Buffer
			err := writeRequest(&output, request)
			if input.name == "invalid-utf8" {
				if err == nil || output.Len() != 0 {
					t.Fatalf("invalid program text was rewritten and sent: %q, %v", output.String(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := readRequest(bufio.NewReader(&output))
			actual, ok := decoded.(launchControlRequest)
			if err != nil || !ok || len(actual.Args) != 1 || actual.Args[0] != input.text {
				t.Fatalf("valid program text changed on the wire: %+v, %v", actual, err)
			}
		})
	}
}

func TestControlLaunchPreservesExactEnvironmentWithoutSocketTransport(t *testing.T) {
	t.Setenv("FRAME_ENV_TEST_MARKER", "parent")
	for _, input := range []struct {
		name string
		env  []string
		want string
	}{
		{name: "absent"},
		{name: "empty", env: []string{}},
		{name: "unicode", env: []string{"FRAME_ENV_TEST_MARKER=original \ufffd \U0001d11e"}, want: "original \ufffd \U0001d11e"},
	} {
		t.Run(input.name, func(t *testing.T) {
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
			request := launchFixture(t, server, "wire-original", 1, t.TempDir())
			request.Args = append(request.Args, "--", "report-environment")
			request.Env = input.env
			host, endpoint := net.Pipe()
			defer host.Close()
			served := make(chan struct{})
			go func() {
				defer close(served)
				defer endpoint.Close()
				server.handle(ctx, endpoint, func() {})
			}()
			if err := writeRequest(host, launchControlRequest{request}); err != nil {
				t.Fatal(err)
			}
			raw, err := readLine(bufio.NewReader(host))
			if err != nil {
				t.Fatal(err)
			}
			response, err := decodeResponse(raw, methodLaunch)
			if err != nil || response == nil || response.(Observation).Reference() != request.AttemptRef {
				t.Fatalf("control launch changed the original attempt: %+v, %v", response, err)
			}
			<-served
			server.mu.Lock()
			live := server.processes[request.AttemptID]
			server.mu.Unlock()
			if live != nil {
				select {
				case <-live.done:
				case <-ctx.Done():
					t.Fatal(context.Cause(ctx))
				}
			}
			observation, err := server.Inspect(ctx, request.AttemptID)
			if err != nil || observation.State != Exited || observation.ExitCode == nil || *observation.ExitCode != 0 {
				t.Fatalf("original child did not complete: %+v, %v", observation, err)
			}
			actual, err := os.ReadFile(filepath.Join(request.Directory, "observed"))
			if err != nil || string(actual) != input.want {
				t.Fatalf("program environment changed: %q, %v", actual, err)
			}
		})
	}
}
