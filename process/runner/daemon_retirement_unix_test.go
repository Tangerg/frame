//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"testing"
	"time"
)

const daemonRetirementFixture = "FRAME_DAEMON_RETIREMENT_FIXTURE"

func TestDaemonRetirementProcess(t *testing.T) {
	if os.Getenv(daemonRetirementFixture) != "1" {
		return
	}
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, os.Interrupt)
	fmt.Fprintln(os.Stdout, "ready")
	<-stopping
	fmt.Fprintln(os.Stdout, "retained")
	<-stopping
	os.Exit(0)
}

func TestDaemonCloseCanAdvanceItsRetainedOriginalShutdown(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestDaemonRetirementProcess$")
	command.Env = append(os.Environ(), daemonRetirementFixture+"=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp("", "frame-close-")
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	original, err := NewDaemon(DaemonConfig{Executable: os.Args[0], Directory: directory, StartTimeout: time.Second})
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	original.started, original.command, original.exited = true, command, make(chan struct{})
	close(original.ready)
	go func() { original.exitErr = command.Wait(); close(original.exited) }()
	t.Cleanup(func() { _ = command.Process.Kill(); <-original.exited })
	lines := make(chan string, 2)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	wait, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	readLine := func(want string) {
		t.Helper()
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("daemon state = %q, want %q", got, want)
			}
		case <-wait.Done():
			t.Fatal(context.Cause(wait))
		}
	}
	readLine("ready")
	abandoned, abandon := context.WithCancelCause(wait)
	cause := errors.New("caller stopped waiting for the retained shutdown")
	first := make(chan error, 1)
	go func() { first <- original.Close(abandoned) }()
	readLine("retained")
	abandon(cause)
	if err := <-first; !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("initial shutdown wait: %v", err)
	}
	select {
	case <-original.closeDone:
		t.Fatal("unconfirmed stop released the original owner")
	default:
	}
	if err := original.Close(wait); err != nil {
		t.Fatalf("a later close could not advance the original shutdown: %v", err)
	}
	if err := original.Close(wait); err != nil {
		t.Fatalf("confirmed exit changed on repeat close: %v", err)
	}
}
