package process_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func echoContract() (*portable.Method, *portable.Capability, error) {
	method, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "document.echo", Kind: portable.Query, Target: "document", Description: "Read the exact document text."})
	if err != nil {
		return nil, nil, err
	}
	contract, err := portable.NewCapability("document.echo", method.Descriptor())
	return method, contract, err
}

type activation struct{ directory string }

func (a activation) Start() error {
	return os.WriteFile(filepath.Join(a.directory, "activated"), []byte("once"), 0600)
}
func (activation) Close() error { return nil }

func TestLocalProcessHelper(t *testing.T) {
	directory := os.Getenv("FRAME_PROCESS_HELPER")
	if directory == "" {
		return
	}
	err := func() error {
		_, contract, err := echoContract()
		if err != nil {
			return err
		}
		return process.ServeStdio(context.Background(), process.Unit{
			ID: "backend", Exports: map[string]*portable.Capability{"echo": contract},
			Prepare: func(ctx context.Context, setup process.Setup) (process.Prepared, error) {
				if err := os.WriteFile(filepath.Join(directory, "prepared"), []byte("once"), 0600); err != nil {
					return process.Prepared{}, err
				}
				if os.Getenv("FRAME_PROCESS_WAIT") == "1" {
					<-ctx.Done()
					return process.Prepared{}, context.Cause(ctx)
				}
				echo := process.Handler(func(ctx context.Context, invocation portable.Invocation) (value.JSON, error) {
					if _, err := os.Stat(filepath.Join(directory, "activated")); err != nil {
						return value.JSON{}, errors.New("business executed before activation")
					}
					return invocation.Input(), nil
				})
				return process.Prepared{Exports: map[string]process.Service{"echo": echo},
					Activate: func(context.Context) error {
						return os.WriteFile(filepath.Join(directory, "activated"), []byte("once"), 0600)
					}}, nil
			},
		})

	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type source struct {
	program      process.Program
	programCalls atomic.Int32
}

func (s *source) Program(context.Context, frame.UnitBinding) (process.Program, error) {
	s.programCalls.Add(1)
	return s.program, nil
}

type environment struct {
	ctx       context.Context
	store     *sqlite.Store
	runner    *runner.Client
	driver    *process.Driver
	host      *frame.Host
	source    *source
	ref       frame.ExportRef
	directory string
}

func newEnvironment(t *testing.T, wait bool) *environment {
	return newConfiguredEnvironment(t, wait, nil, nil)
}

func newConfiguredEnvironment(t *testing.T, wait bool, protocol process.Protocol, closeDiagnostic error) *environment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	directory, err := os.MkdirTemp("", "frame-process-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	server, err := runner.Open(runner.Config{Directory: filepath.Join(directory, "runner")})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := server.Listen(filepath.Join(directory, "runner", "socket"))
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-served
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	client := runner.Dial(filepath.Join(directory, "runner", "socket"))
	admission := frame.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(directory, "state.sqlite"), Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := client.Fence(ctx, store.Epoch()); err != nil {
		t.Fatal(err)
	}
	_, capability, err := echoContract()
	if err != nil {
		t.Fatal(err)
	}
	contract, err := frame.NewPortableContract[process.Service]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	ref := contract.Ref()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	source := &source{program: process.Program{Command: executable, ExecutableDigest: value.Hash(data), Args: []string{"-test.run=^TestLocalProcessHelper$"}, Env: append(os.Environ(), "FRAME_PROCESS_HELPER="+directory), Directory: directory}}
	if wait {
		source.program.Env = append(source.program.Env, "FRAME_PROCESS_WAIT=1")
	}
	driver, err := process.New(process.Config{Runner: client, Store: store, Source: source, CloseTimeout: 3 * time.Second, MaxFrameBytes: 1 << 20, Protocol: protocol})
	if err != nil {
		t.Fatal(err)
	}
	host, err := frame.NewHost(ctx, frame.HostConfig{Admission: admission, Epoch: store.Epoch(), Drivers: []frame.Driver{driver}, Installations: store, ExecutionProtection: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		outcome, err := host.Close(closeCtx)
		if !outcome.Released || err != nil && closeDiagnostic == nil {
			t.Errorf("host release: %+v %v", outcome, err)
		}
		if closeDiagnostic != nil {
			if len(outcome.Failures) == 0 {
				t.Error("original close diagnostic was lost")
			}
			for _, failure := range outcome.Failures {
				if !strings.Contains(failure.Detail, closeDiagnostic.Error()) {
					t.Errorf("unexpected close diagnostic: %+v", failure)
				}
			}
		}
	})
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("echo release")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "backend", Profile: frame.LocalProcess, Exports: []frame.ExportDeclaration{{Name: "echo", Contract: ref}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "echo", Plugin: plugin, Configuration: []byte(`{}`), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return &environment{ctx: ctx, store: store, runner: client, driver: driver, host: host, source: source, ref: frame.ExportRef{InstallationID: "echo", Release: plugin.Revision(), UnitID: "backend", Export: "echo", Contract: ref}, directory: directory}
}

func TestApprovedProcessUsesPublicContractAndReleasesOriginalAttempt(t *testing.T) {
	e := newEnvironment(t, false)
	binding, err := e.host.Plan(e.ctx, e.ref)
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(e.ctx)
	scope, err := e.host.Admission().OpenScope(e.ctx, "document")
	if err != nil {
		t.Fatal(err)
	}
	use, err := e.host.Acquire(e.ctx, binding, frame.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	input, err := value.EncodeJSON("document exact text")
	if err != nil {
		t.Fatal(err)
	}
	err = frame.BorrowExport[process.Service](e.ctx, use, e.ref, func(ctx context.Context, capability process.Service) error {
		invocation, err := portable.NewQueryInvocation("document.echo", input)
		if err != nil {
			return err
		}
		result, err := capability.Invoke(ctx, invocation)
		if err == nil && result != input {
			return fmt.Errorf("reply changed: %s", result.String())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := use.Close(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Close(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.host.SetEnabled(e.ctx, "echo", false); err != nil {
		t.Fatal(err)
	}
	if err := e.host.WaitRetired(e.ctx, "echo"); err != nil {
		t.Fatal(err)
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || len(attempts) != 1 || !attempts[0].Released || attempts[0].Proof == nil {
		t.Fatalf("original attempt release: %+v %v", attempts, err)
	}
	observed, err := e.runner.Inspect(e.ctx, attempts[0].Attempt.ID)
	if err != nil || observed.State != runner.Exited || observed.ProofID != attempts[0].Proof.ExitID {
		t.Fatalf("actual runner exit: %+v %v", observed, err)
	}
	if held, err := e.store.ExecutionDependencies(e.ctx); err != nil || len(held) != 0 {
		t.Fatalf("released execution retained dependencies: %+v %v", held, err)
	}
	if e.source.programCalls.Load() != 1 {
		t.Fatal("acquiring the original binding created a second process")
	}
}

func TestDisableDuringRealProcessSetupCannotPublishOrActivate(t *testing.T) {
	e := newEnvironment(t, true)
	result := make(chan error, 1)
	go func() { _, err := e.host.Activate(e.ctx, e.ref); result <- err }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	waiting := true
	for waiting {
		select {
		case <-e.ctx.Done():
			t.Fatal(context.Cause(e.ctx))
		case err := <-result:
			t.Fatalf("setup did not remain preparing: %v", err)
		case <-ticker.C:
			_, err := os.Stat(filepath.Join(e.directory, "prepared"))
			waiting = errors.Is(err, os.ErrNotExist)
			if err != nil && !waiting {
				t.Fatal(err)
			}
		}
	}
	if err := e.host.SetEnabled(e.ctx, "echo", false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("withdrawn preparation published")
		}
	case <-e.ctx.Done():
		t.Fatal(context.Cause(e.ctx))
	}
	if err := e.host.WaitRetired(e.ctx, "echo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.directory, "activated")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("withdrawn setup activated: %v", err)
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || len(attempts) != 1 || !attempts[0].Released {
		t.Fatalf("late preparation lost protection: %+v %v", attempts, err)
	}
}
