package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

func launchFixture(t *testing.T, owner interface {
	Identity(context.Context) (Identity, error)
}, id string, epoch uint64, directory string) LaunchRequest {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := owner.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return LaunchRequest{AttemptRef: AttemptRef{AttemptID: id, Epoch: epoch, RunnerID: authority.ID}, Command: executable, ExecutableDigest: framevalue.Hash(data), Configuration: framevalue.Hash([]byte(directory)), Args: []string{"-test.run=^TestRunnerProcessHelper$"}, Env: append(os.Environ(), "FRAME_RUNNER_HELPER=echo", "FRAME_RUNNER_COUNT="+filepath.Join(directory, "created")), Directory: directory}
}

func serveFixture(t *testing.T) (*Server, *Client, context.Context) {
	t.Helper()
	// Unix-domain paths have an OS limit independent of an archive's path limit.
	directory, err := os.MkdirTemp("", "frame-runner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	server, err := Open(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := server.Listen(filepath.Join(directory, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-done
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	return server, Dial(filepath.Join(directory, "socket")), ctx
}

func TestAttemptStartsOnceAndAttachCarriesActualProcessIO(t *testing.T) {
	server, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "convert-1", 1, t.TempDir())
	observation, err := client.Launch(ctx, request)
	if err != nil || observation.State != Running {
		t.Fatalf("launch: %+v %v", observation, err)
	}
	stream, err := client.Attach(ctx, request.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("exact binding\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stream).ReadString('\n')
	if err != nil || line != "exact binding\n" {
		t.Fatalf("process stream: %q %v", line, err)
	}
	if _, err := client.Attach(ctx, request.AttemptRef); !errors.Is(err, ErrAttached) {
		t.Fatalf("second attachment: %v", err)
	}
	changed := request
	changed.Configuration = framevalue.Hash([]byte("another configuration"))
	if _, err := client.Launch(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed attempt: %v", err)
	}
	closed, err := client.CloseAttempt(ctx, request.AttemptRef)
	if err != nil || closed.State != Exited || closed.ProofID == "" {
		t.Fatalf("exit: %+v %v", closed, err)
	}
	_ = stream.Close()
	again, err := client.Launch(ctx, request)
	if err != nil || again.ProofID != closed.ProofID || again.State != Exited {
		t.Fatalf("old launch restarted: %+v %v", again, err)
	}
	count, err := os.ReadFile(filepath.Join(request.Directory, "created"))
	if err != nil || string(count) != "created\n" {
		t.Fatalf("creation count: %q %v", count, err)
	}
	if _, err := os.Stat(filepath.Join(server.config.Directory, "attempts.json")); err != nil {
		t.Fatal(err)
	}
}

func TestFenceSettlesOldCreationAndKeepsOriginalExitReachable(t *testing.T) {
	_, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "old", 1, t.TempDir())
	if _, err := client.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := client.Fence(ctx, 2); err != nil {
		t.Fatal(err)
	}
	late := request
	late.AttemptID = "late-old"
	if _, err := client.Launch(ctx, late); !errors.Is(err, ErrFenced) {
		t.Fatalf("late old creation: %v", err)
	}
	if _, err := client.CloseAttempt(ctx, request.AttemptRef); err != nil {
		t.Fatal(err)
	}
	current := request
	current.AttemptID = "current"
	current.Epoch = 2
	if _, err := client.Launch(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CloseAttempt(ctx, current.AttemptRef); err != nil {
		t.Fatal(err)
	}
}

func TestFailedExecutableIsTerminalAndNeverStartsOnRetry(t *testing.T) {
	_, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "bad", 1, t.TempDir())
	request.ExecutableDigest = framevalue.Hash([]byte("not the executable"))
	first, err := client.Launch(ctx, request)
	if err == nil || first.State != Exited || first.ProofID == "" {
		t.Fatalf("bad executable: %+v %v", first, err)
	}
	second, err := client.Launch(ctx, request)
	if err != nil || second.ProofID != first.ProofID {
		t.Fatalf("retry changed failure: %+v %v", second, err)
	}
	if _, err := os.Stat(filepath.Join(request.Directory, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected execution: %v", err)
	}
}

func executableSnapshots(t *testing.T, server *Server) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(server.config.Directory, "executable-*"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestFailedProcessCreationReleasesVerifiedExecutable(t *testing.T) {
	for _, failure := range []string{"invalid-format", "missing-directory"} {
		t.Run(failure, func(t *testing.T) {
			server, client, ctx := serveFixture(t)
			if err := client.Fence(ctx, 1); err != nil {
				t.Fatal(err)
			}
			request := launchFixture(t, client, failure, 1, t.TempDir())
			if failure == "invalid-format" {
				data := []byte("not an executable format\n")
				request.Command = filepath.Join(request.Directory, "invalid")
				request.ExecutableDigest = framevalue.Hash(data)
				if err := os.WriteFile(request.Command, data, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				request.Directory = filepath.Join(request.Directory, "missing")
			}
			observation, err := client.Launch(ctx, request)
			if err == nil || observation.State != Exited || observation.ProofID == "" || observation.Reason != "process creation failed" {
				t.Fatalf("failed creation: %+v, %v", observation, err)
			}
			if paths := executableSnapshots(t, server); len(paths) != 0 {
				t.Fatalf("failed launch retained executable snapshots: %v", paths)
			}
			if err := server.Close(); err != nil {
				t.Fatal(err)
			}
			if paths := executableSnapshots(t, server); len(paths) != 0 {
				t.Fatalf("closed runner retained executable snapshots: %v", paths)
			}
		})
	}
}

func TestChildWaitOwnsExecutableReleaseAndReportsCleanupFailure(t *testing.T) {
	for _, refuseRemoval := range []bool{false, true} {
		t.Run(fmt.Sprint(refuseRemoval), func(t *testing.T) {
			server, client, ctx := serveFixture(t)
			if err := client.Fence(ctx, 1); err != nil {
				t.Fatal(err)
			}
			request := launchFixture(t, client, "live-snapshot", 1, t.TempDir())
			if _, err := client.Launch(ctx, request); err != nil {
				t.Fatal(err)
			}
			paths := executableSnapshots(t, server)
			if len(paths) != 1 {
				t.Fatalf("running child does not retain its snapshot: %v", paths)
			}
			if refuseRemoval {
				if err := os.Rename(paths[0], paths[0]+".held"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(paths[0], 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(paths[0], "obstacle"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			observation, err := client.CloseAttempt(ctx, request.AttemptRef)
			if err != nil || observation.State != Exited || observation.ProofID == "" {
				t.Fatalf("child exit: %+v, %v", observation, err)
			}
			if refuseRemoval {
				if !strings.Contains(observation.Reason, "executable snapshot cleanup failed") {
					t.Fatalf("snapshot removal failure was hidden: %+v", observation)
				}
				persisted, err := client.Inspect(ctx, request.AttemptID)
				if err != nil || persisted.Reason != observation.Reason {
					t.Fatalf("cleanup diagnostic was not retained: %+v, %v", persisted, err)
				}
			} else if paths := executableSnapshots(t, server); len(paths) != 0 {
				t.Fatalf("completed wait retained executable snapshots: %v", paths)
			}
		})
	}
}

func TestSeparateRunnerSurvivesClientAndLosesNoAttemptOnRestart(t *testing.T) {
	directory, err := os.MkdirTemp("", "frame-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	command := exec.Command(os.Args[0], "-test.run=^TestRunnerProcessHelper$")
	command.Env = append(os.Environ(), "FRAME_RUNNER_HELPER=daemon", "FRAME_RUNNER_DIRECTORY="+directory)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		_ = command.Process.Kill()
		waitErr := command.Wait()
		command.Process = nil
		t.Fatalf("daemon ready: %q %v %s", line, errors.Join(err, waitErr), diagnostics.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := Dial(filepath.Join(directory, "socket"))
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "retained-attempt", 1, t.TempDir())
	first, err := client.Launch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// A new Host connection observes the original process, without receiving any
	// launch handle or replaying the effect.
	reconnected := Dial(filepath.Join(directory, "socket"))
	seen, err := reconnected.Inspect(ctx, request.AttemptID)
	if err != nil || seen.PID != first.PID || seen.RunnerID != first.RunnerID {
		t.Fatalf("reconnected observation: %+v %v", seen, err)
	}
	stream, err := reconnected.Attach(ctx, request.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("created\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stream).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("runner did not crash")
	}
	command.Process = nil
	_ = stream.Close()
	server, err := Open(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.Fence(ctx, 2); err != nil {
		t.Fatal(err)
	}
	old, err := server.Launch(ctx, request)
	if err != nil || old.State != Unknown || old.PID != 0 || old.ProofID != "" {
		t.Fatalf("restart fabricated ownership: %+v %v", old, err)
	}
	if _, err := server.CloseAttempt(ctx, request.AttemptRef); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown process was killed by stale identity: %v", err)
	}
	count, err := os.ReadFile(filepath.Join(request.Directory, "created"))
	if err != nil || string(count) != "created\n" {
		t.Fatalf("restart created another child: %q %v", count, err)
	}
	stale := request
	stale.AttemptID = "missing-old-attempt"
	if _, err := server.Launch(ctx, stale); !errors.Is(err, ErrFenced) {
		t.Fatalf("missing old attempt restarted: %v", err)
	}
}

func TestRunnerProcessHelper(t *testing.T) {
	// Arguments select this mode without adding variables to an empty environment.
	if flag.Arg(0) == "report-environment" {
		if err := os.WriteFile("observed", []byte(os.Getenv("FRAME_ENV_TEST_MARKER")), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	switch os.Getenv("FRAME_RUNNER_HELPER") {
	case "reply-and-exit":
		var input [1]byte
		if _, err := io.ReadFull(os.Stdin, input[:]); err != nil {
			os.Exit(33)
		}
		fmt.Println("final")
		os.Exit(0)
	case "wait-for-kill":
		fmt.Println("ready")
		for {
			time.Sleep(time.Hour)
		}
	case "echo":
		file, err := os.OpenFile(os.Getenv("FRAME_RUNNER_COUNT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(31)
		}
		_, err = file.WriteString("created\n")
		if err != nil {
			os.Exit(32)
		}
		_ = file.Sync()
		_ = file.Close()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
		os.Exit(0)
	case "daemon":
		directory := os.Getenv("FRAME_RUNNER_DIRECTORY")
		server, err := Open(Config{Directory: directory})
		if err != nil {
			panic(err)
		}
		listener, err := server.Listen(filepath.Join(directory, "socket"))
		if err != nil {
			panic(err)
		}
		fmt.Println("ready")
		if err := server.Serve(context.Background(), listener); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
}

func TestJournalContainsNoEnvironmentSecrets(t *testing.T) {
	server, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "secret", 1, t.TempDir())
	request.CredentialRevision = "credential-1"
	request.Env = append(request.Env, "FRAME_TEST_PRIVATE_TOKEN=must-not-be-persisted")
	if _, err := client.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CloseAttempt(ctx, request.AttemptRef); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(server.config.Directory, "attempts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "must-not-be-persisted") || strings.Contains(string(data), "FRAME_TEST_PRIVATE_TOKEN") {
		t.Fatal("secret environment was persisted")
	}
	var value journal
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentDisconnectClosesOriginalProcess(t *testing.T) {
	_, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, client, "disconnect", 1, t.TempDir())
	if _, err := client.Launch(ctx, request); err != nil {
		t.Fatal(err)
	}
	stream, err := client.Attach(ctx, request.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stream, "ready\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stream).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	observation, err := client.CloseAttempt(ctx, request.AttemptRef)
	if err != nil || observation.State != Exited || observation.ProofID == "" {
		t.Fatalf("disconnect did not settle original exit: %+v %v", observation, err)
	}
}

func TestExplicitEmptyEnvironmentSurvivesTransport(t *testing.T) {
	t.Setenv("FRAME_ENV_TEST_MARKER", "parent")
	_, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"default-empty", nil, ""}, {"empty", []string{}, ""}, {"explicit", []string{"FRAME_ENV_TEST_MARKER=child"}, "child"},
	} {
		request := launchFixture(t, client, tc.name, 1, t.TempDir())
		request.Env = tc.env
		request.Args = append(request.Args, "--", "report-environment")
		if _, err := client.Launch(ctx, request); err != nil {
			t.Fatal(err)
		}
		for {
			o, err := client.Inspect(ctx, request.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if o.State == Exited {
				if o.ExitCode == nil || *o.ExitCode != 0 {
					t.Fatalf("%s: helper failed: %+v", tc.name, o)
				}
				break
			}
			time.Sleep(time.Millisecond)
		}
		got, err := os.ReadFile(filepath.Join(request.Directory, "observed"))
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s: %q %v", tc.name, got, err)
		}
		if tc.env == nil {
			request.Env = []string{}
			if _, err := client.Launch(ctx, request); err != nil {
				t.Fatalf("equivalent empty environment: %v", err)
			}
		}
	}
}

func TestReturnedExitIsDetachedFromJournal(t *testing.T) {
	s, c, ctx := serveFixture(t)
	if err := c.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	r := launchFixture(t, c, "exit-alias", 1, t.TempDir())
	if _, err := c.Launch(ctx, r); err != nil {
		t.Fatal(err)
	}
	o, err := s.CloseAttempt(ctx, r.AttemptRef)
	if err != nil || o.ExitCode == nil {
		t.Fatal(err)
	}
	original := *o.ExitCode
	*o.ExitCode = 101
	again, err := s.Inspect(ctx, r.AttemptID)
	if err != nil || *again.ExitCode != original {
		t.Fatalf("borrowed journal value: %+v %v", again, err)
	}
}

func TestFailedStartRequiresDurableExitBeforePublishingProof(t *testing.T) {
	s, c, ctx := serveFixture(t)
	if err := c.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	r := launchFixture(t, c, "failed-proof", 1, t.TempDir())
	digest, err := requestIdentity(r)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.journal.Attempts[r.AttemptID] = storedAttempt{
		Observation:   Observation{AttemptID: r.AttemptID, Epoch: r.Epoch, RunnerID: r.RunnerID, State: Starting},
		RequestDigest: digest,
	}
	err = s.persist()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restore := interruptJournal(t, s)
	s.mu.Lock()
	o, err := s.failedStart(r.AttemptID, "fixture refused start", errors.New("invalid executable"))
	original := s.terminalObservations[r.AttemptID].clone()
	s.mu.Unlock()
	if !errors.Is(err, ErrUnknown) || o.State != Unknown || o.ProofID != "" {
		t.Fatalf("failed persist: %+v %v", o, err)
	}
	if o, err := c.Inspect(ctx, r.AttemptID); !errors.Is(err, ErrUnknown) || o.State != Unknown || o.ProofID != "" {
		t.Fatalf("inspect: %+v %v", o, err)
	}
	if o, err := c.CloseAttempt(ctx, r.AttemptRef); !errors.Is(err, ErrUnknown) || o.ProofID != "" {
		t.Fatalf("close: %+v %v", o, err)
	}
	restore()
	if o, err := c.Launch(ctx, r); err != nil || o.State != Exited || o.ProofID == "" || o.ProofID != original.ProofID {
		t.Fatalf("failed start replay changed the original evidence: %+v %v", o, err)
	}
	if _, err := os.Stat(filepath.Join(r.Directory, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed start replay created a child: %v", err)
	}
}

func TestOnlyAcceptedExitReleasesCapacityAndPreventsReplay(t *testing.T) {
	s, c, ctx := serveFixture(t)
	s.config.MaxAttempts = 1
	if err := c.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	first := launchFixture(t, c, "first", 1, t.TempDir())
	if _, err := c.Launch(ctx, first); err != nil {
		t.Fatal(err)
	}
	o, err := c.CloseAttempt(ctx, first.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	next := first
	next.AttemptID = "second"
	if _, err := c.Launch(ctx, next); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("unclaimed exit lost protection: %v", err)
	}
	receipt := ExitReceipt{AttemptID: o.AttemptID, Epoch: o.Epoch, RunnerID: o.RunnerID, ProofID: o.ProofID}
	wrong := receipt
	wrong.ProofID += "other"
	if err := c.AcknowledgeExit(ctx, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.AcknowledgeExit(ctx, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Launch(ctx, next); err != nil {
		t.Fatal(err)
	}
	replayed, err := c.Launch(ctx, first)
	if err != nil || replayed.ProofID != o.ProofID {
		t.Fatalf("original replay: %+v %v", replayed, err)
	}
	if err := c.Fence(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProveAbsent(ctx, AttemptRef{AttemptID: o.AttemptID, Epoch: o.Epoch, RunnerID: o.RunnerID}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("false absence: %v", err)
	}
	if _, err := c.CloseAttempt(ctx, next.AttemptRef); err != nil {
		t.Fatal(err)
	}
}

func TestHandedOffIdentitySurvivesRunnerRestartAndNewEpoch(t *testing.T) {
	ctx := t.Context()
	cfg := Config{Directory: t.TempDir(), MaxAttempts: 1}
	open := func(epoch uint64) *Server {
		s, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := s.Fence(ctx, epoch); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := open(1)
	req := launchFixture(t, first, "retained-attempt", 1, t.TempDir())
	if _, err := first.Launch(ctx, req); err != nil {
		t.Fatal(err)
	}
	exit, err := first.CloseAttempt(ctx, req.AttemptRef)
	if err != nil {
		t.Fatal(err)
	}
	receipt := ExitReceipt{AttemptID: exit.AttemptID, Epoch: exit.Epoch, RunnerID: exit.RunnerID, ProofID: exit.ProofID}
	if err := first.AcknowledgeExit(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := open(2)
	// The first acknowledgement reply was lost before the Host confirmed it.
	if err := replacement.AcknowledgeExit(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	replayed, err := replacement.Launch(ctx, req)
	if err != nil || replayed.State != Exited || replayed.ProofID != exit.ProofID {
		t.Fatalf("original receipt: %+v %v", replayed, err)
	}
	req.Epoch = 2
	if _, err := replacement.Launch(ctx, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("repurposed attempt ID: %v", err)
	}
	req.AttemptID = "new-attempt"
	req.RunnerID = replacement.id
	if o, err := replacement.Launch(ctx, req); err != nil || o.State != Running {
		t.Fatalf("free capacity: %+v %v", o, err)
	}
}
