package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/value"
)

type shutdownDiagnostic struct{ observed chan string }

func (d shutdownDiagnostic) Write(data []byte) (int, error) {
	d.observed <- string(data)
	return len(data), nil
}

func TestShutdownRetainsOriginalAuthorityWithoutControlTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	directory := t.TempDir()
	server, err := runner.Open(runner.Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	identity, err := server.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	ref := runner.AttemptRef{AttemptID: "original-child", Epoch: 1, RunnerID: identity.ID}
	if _, err := server.Launch(ctx, runner.LaunchRequest{AttemptRef: ref, Command: "/bin/sleep", ExecutableDigest: value.Hash(program), Args: []string{"20"}, Configuration: value.Hash([]byte("shutdown"))}); err != nil {
		t.Fatal(err)
	}
	journal, saved := filepath.Join(directory, "attempts.json"), filepath.Join(directory, "saved-attempts.json")
	if err := os.Rename(journal, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journal, 0700); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Remove(journal); err != nil {
			t.Error(err)
		}
		if err := os.Rename(saved, journal); err != nil {
			t.Error(err)
		}
	})
	signals := make(chan os.Signal, 1)
	diagnostic := shutdownDiagnostic{observed: make(chan string, 1)}
	done := make(chan struct{})
	var closeErr error
	go func() { closeErr = shutdown(server, signals, diagnostic); close(done) }()
	t.Cleanup(func() {
		restore()
		select {
		case <-done:
			return
		case signals <- os.Interrupt:
		}
		<-done
	})
	select {
	case line := <-diagnostic.observed:
		if !strings.Contains(line, "shutdown remains unconfirmed") {
			t.Fatal(line)
		}
	case <-done:
		t.Fatalf("shutdown discarded retained authority: %v", closeErr)
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	if replacement, err := runner.Open(runner.Config{Directory: directory}); err == nil {
		_ = replacement.Close()
		t.Fatal("unconfirmed shutdown handed off directory authority")
	}
	restore()
	signals <- os.Interrupt
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	reopened, err := runner.Open(runner.Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	observed, err := reopened.Inspect(ctx, ref.AttemptID)
	if err != nil || observed.State != runner.Exited || observed.ProofID == "" || observed.RunnerID != identity.ID {
		t.Fatalf("original proof lost: %+v %v", observed, err)
	}
}

func TestExecutableRetainsOriginalWaitAuthorityUntilJournalRecovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	executable := filepath.Join(t.TempDir(), "frame-runner")
	build := exec.CommandContext(ctx, "go", "build", "-o", executable, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}
	directory, err := os.MkdirTemp("", "frame-shutdown-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "socket")
	command := exec.Command(executable, "--directory", directory, "--socket", socket)
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := make(chan string, 2)
	go func() {
		defer close(diagnostics)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			diagnostics <- scanner.Text()
		}
	}()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var exitErr error
	go func() { exitErr = command.Wait(); close(exited) }()
	t.Cleanup(func() { _ = command.Process.Kill(); <-exited })
	client := runner.Dial(socket)
	var identity runner.Identity
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		identity, err = client.Identity(ctx)
		if err == nil {
			break
		}
		select {
		case <-exited:
			t.Fatalf("executable exited before readiness: %v", exitErr)
		case <-ctx.Done():
			t.Fatal(context.Cause(ctx))
		case <-ticker.C:
		}
	}
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	ref := runner.AttemptRef{AttemptID: "original-child", Epoch: 1, RunnerID: identity.ID}
	request := runner.LaunchRequest{AttemptRef: ref, Command: "/bin/sleep", ExecutableDigest: value.Hash(program), Args: []string{"20"}, Configuration: value.Hash([]byte("shutdown"))}
	if observed, err := client.Launch(ctx, request); err != nil || observed.State != runner.Running {
		t.Fatalf("launch original child: %+v %v", observed, err)
	}
	journal := filepath.Join(directory, "attempts.json")
	saved := filepath.Join(directory, "saved-attempts.json")
	if err := os.Rename(journal, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-diagnostics:
		if !strings.Contains(line, "shutdown remains unconfirmed") {
			t.Fatalf("missing unfinished-shutdown diagnostic: %q", line)
		}
	case <-exited:
		t.Fatalf("unconfirmed shutdown destroyed its original owner: %v", exitErr)
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	select {
	case <-exited:
		t.Fatalf("unconfirmed shutdown destroyed its original owner: %v", exitErr)
	default:
	}
	if replacement, err := runner.Open(runner.Config{Directory: directory}); err == nil {
		_ = replacement.Close()
		t.Fatal("unconfirmed shutdown released directory authority")
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, journal); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
		if exitErr != nil {
			t.Fatalf("confirmed shutdown failed: %v", exitErr)
		}
	case <-ctx.Done():
		t.Fatal("restored journal did not complete original shutdown")
	}
	reopened, err := runner.Open(runner.Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	observed, err := reopened.Inspect(ctx, ref.AttemptID)
	if err != nil || observed.State != runner.Exited || observed.ProofID == "" || observed.RunnerID != identity.ID {
		t.Fatalf("original Wait proof did not survive shutdown: %+v %v", observed, err)
	}
	if _, err := os.Stat(saved); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery left a second journal: %v", err)
	}
}
