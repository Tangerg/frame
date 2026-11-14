package process_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type settlementRunner struct {
	*runner.Server
	launch func(context.Context, runner.LaunchRequest) (runner.LaunchResult, error)
	calls  atomic.Int32
}

func (r *settlementRunner) Launch(ctx context.Context, request runner.LaunchRequest) (runner.LaunchResult, error) {
	r.calls.Add(1)
	if r.launch != nil {
		return r.launch(ctx, request)
	}
	return r.Server.Launch(ctx, request)
}

func (*settlementRunner) Attach(context.Context, runner.AttemptRef) (io.ReadWriteCloser, error) {
	return nil, errors.New("fixture does not attach a refused launch")
}

type settlementEnvironment struct {
	host            framehost.Host
	store           sqlite.Store
	driver          *process.Driver
	runner          *settlementRunner
	source          *source
	unit            framehost.UnitRef
	contentReleased atomic.Int32
}

func newSettlementEnvironment(t *testing.T) *settlementEnvironment {
	t.Helper()
	e := &settlementEnvironment{}
	server, err := runner.Open(runner.Config{Directory: filepath.Join(t.TempDir(), "runner"), MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	e.runner = &settlementRunner{Server: server}
	admission := framehost.NewAdmission()
	e.store, err = sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.store.Close(); err != nil {
			t.Error(err)
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	e.source = &source{program: process.Program{
		Command: executable, ExecutableDigest: value.Hash(data), Args: []string{"-test.run=^TestLocalProcessHelper$"},
		ReleaseContent: func() error { e.contentReleased.Add(1); return nil },
	}}
	e.driver, err = process.New(process.Config{Runner: e.runner, Store: e.store, Source: e.source, CloseTimeout: time.Second, MaxFrameBytes: 4096, Protocol: cleanupProtocol{connection: &cleanupConnection{}}})
	if err != nil {
		t.Fatal(err)
	}
	e.host, err = framehost.New(t.Context(), framehost.Config{Authority: e.store, Drivers: []framehost.Driver{e.driver}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if outcome, err := e.host.Close(ctx); !outcome.Released {
			t.Errorf("host close: %+v %v", outcome, err)
		}
	})
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash(nil), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.LocalProcess}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.host.Install(t.Context(), framehost.InstallationConfig{ID: "provider", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.unit = framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
	return e
}

func (e *settlementEnvironment) assertRunnerExit(t *testing.T, state runner.State) {
	t.Helper()
	e.assertReleased(t, "runner-exit")
	records, err := e.store.Attempts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var receipt runner.ExitReceipt
	if err := json.Unmarshal(records[0].Proof.Evidence, &receipt); err != nil || receipt.State != state {
		t.Fatalf("runner proof settled %q, want %q: %v", receipt.State, state, err)
	}
}

func (e *settlementEnvironment) assertReleased(t *testing.T, kind string) {
	t.Helper()
	wait, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		outcome, err := e.host.Close(wait)
		if outcome.Released {
			break
		}
		select {
		case <-tick.C:
		case <-wait.Done():
			t.Fatalf("original resources retained: %+v %v", outcome, err)
		}
	}
	records, err := e.store.Attempts(t.Context())
	if err != nil || len(records) != 1 || records[0].State != sqlite.AttemptReleased || records[0].Proof == nil || records[0].Proof.Kind != kind {
		t.Fatalf("original attempt not settled by %s: %+v %v", kind, records, err)
	}
	dependencies, err := e.store.ExecutionDependencies(t.Context())
	if err != nil || len(dependencies) != 0 || e.contentReleased.Load() != 1 || e.runner.calls.Load() != 1 {
		t.Fatalf("release ownership: dependencies=%+v released=%d launches=%d error=%v", dependencies, e.contentReleased.Load(), e.runner.calls.Load(), err)
	}
}

func TestInvalidProgramDoesNotRecordAnAttempt(t *testing.T) {
	e := newSettlementEnvironment(t)
	e.source.program.Args = []string{"\xff"}
	if _, err := e.host.ActivateUnit(t.Context(), "application", e.unit); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("program admission: %v", err)
	}
	if outcome, err := e.host.Close(t.Context()); !outcome.Released {
		t.Fatalf("invalid program cleanup: %+v %v", outcome, err)
	}
	records, err := e.store.Attempts(t.Context())
	if err != nil || len(records) != 0 || e.runner.calls.Load() != 0 || e.contentReleased.Load() != 1 {
		t.Fatalf("invalid program acquired durable creation responsibility: %+v launches=%d released=%d error=%v", records, e.runner.calls.Load(), e.contentReleased.Load(), err)
	}
}

func TestOriginalUndispatchedLaunchReleasesApprovedAttempt(t *testing.T) {
	e := newSettlementEnvironment(t)
	cause := errors.New("cancelled immediately after durable approval")
	e.runner.launch = func(ctx context.Context, request runner.LaunchRequest) (runner.LaunchResult, error) {
		wait, cancel := context.WithCancelCause(ctx)
		cancel(cause)
		return e.runner.Server.Launch(wait, request)
	}
	if _, err := e.host.ActivateUnit(t.Context(), "application", e.unit); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("original cancellation: %v", err)
	}
	e.assertReleased(t, "not-dispatched")
}

func TestCapacityRefusalSettlesTheOriginalAttemptInTheCurrentEpoch(t *testing.T) {
	e := newSettlementEnvironment(t)
	if err := e.runner.Fence(t.Context(), e.store.Epoch()); err != nil {
		t.Fatal(err)
	}
	identity, err := e.runner.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	program := e.source.program
	occupied := runner.LaunchRequest{AttemptRef: runner.AttemptRef{AttemptID: "occupied", Epoch: identity.Epoch, RunnerID: identity.ID}, Command: program.Command, ExecutableDigest: program.ExecutableDigest, Args: program.Args, Configuration: value.Hash(nil)}
	if _, err := e.runner.Server.Launch(t.Context(), occupied); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner.CloseAttempt(t.Context(), occupied.AttemptRef); err != nil {
		t.Fatal(err)
	}
	if _, err := e.host.ActivateUnit(t.Context(), "application", e.unit); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("capacity refusal: %v", err)
	}
	e.assertRunnerExit(t, runner.NotCreated)
}

func TestReconcileNeverReplacesAnUnknownLaunchWithAnUndispatchedRetry(t *testing.T) {
	e := newSettlementEnvironment(t)
	var original runner.LaunchRequest
	e.runner.launch = func(ctx context.Context, request runner.LaunchRequest) (runner.LaunchResult, error) {
		if e.runner.calls.Load() == 1 {
			original = request
			return runner.LaunchResult{}, runner.ErrUnknown
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return e.runner.Server.Launch(cancelled, request)
	}
	if _, err := e.host.ActivateUnit(t.Context(), "application", e.unit); !errors.Is(err, runner.ErrUnknown) {
		t.Fatalf("unknown launch: %v", err)
	}
	if outcome, _ := e.host.Close(t.Context()); outcome.Released {
		t.Fatal("unknown dispatch released its original dependency")
	}
	if err := e.driver.Reconcile(t.Context(), original.AttemptID); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("missing original acceptance became evidence: %v", err)
	}
	records, err := e.store.Attempts(t.Context())
	dependencies, dependenciesErr := e.store.ExecutionDependencies(t.Context())
	if err != nil || dependenciesErr != nil || len(records) != 1 || records[0].ExitProven() || len(dependencies) != 1 || e.contentReleased.Load() != 0 || e.runner.calls.Load() != 1 {
		t.Fatalf("unknown attempt changed: %+v dependencies=%+v launches=%d released=%d errors=%v/%v", records, dependencies, e.runner.calls.Load(), e.contentReleased.Load(), err, dependenciesErr)
	}
	if _, err := e.runner.Server.Launch(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if err := e.driver.Reconcile(t.Context(), original.AttemptID); err != nil {
		t.Fatalf("late original acceptance: %v", err)
	}
	e.assertRunnerExit(t, runner.Exited)
}
