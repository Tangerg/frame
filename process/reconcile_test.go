package process_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/value"
)

type cleanupProtocol struct{ connection *cleanupConnection }

func (cleanupProtocol) Profile() frame.Profile { return frame.LocalProcess }
func (p cleanupProtocol) Prepare(setup *frame.Setup, metadata process.ExportMetadata, _ int) (process.Connection, error) {
	for _, export := range setup.Unit().Exports {
		p.connection.values = append(p.connection.values, metadata.Export(export, process.Handler(func(_ context.Context, invocation portable.Invocation) (value.JSON, error) {
			return invocation.Input(), nil
		})))
	}
	return p.connection, nil
}

type cleanupConnection struct {
	values      []frame.ExportValue
	started     chan struct{}
	finishClose func(context.Context) error
	connect     func(context.Context) error
	closes      atomic.Int32
}

func (c *cleanupConnection) Connect(ctx context.Context, _ io.ReadWriteCloser) ([]frame.ExportValue, error) {
	if c.connect != nil {
		if err := c.connect(ctx); err != nil {
			return nil, err
		}
	}
	return c.values, nil
}
func (c *cleanupConnection) Close(ctx context.Context) error {
	c.closes.Add(1)
	if c.started != nil {
		close(c.started)
	}
	if c.finishClose != nil {
		return c.finishClose(ctx)
	}
	return nil
}

func originalAttempt(t *testing.T, e *environment) runner.AttemptRef {
	t.Helper()
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("original attempt: %+v %v", attempts, err)
	}
	attempt := attempts[0].Attempt
	return runner.AttemptRef{AttemptID: attempt.ID, Epoch: attempt.Epoch, RunnerID: attempt.Realm}
}

func TestReconcileRefusesChildProofBeforeOriginalProtocolCleanup(t *testing.T) {
	connection := &cleanupConnection{}
	e := newConfiguredEnvironment(t, false, cleanupProtocol{connection}, nil)
	if _, err := e.host.Activate(e.ctx, e.ref); err != nil {
		t.Fatal(err)
	}
	ref := originalAttempt(t, e)
	if _, err := e.runner.CloseAttempt(e.ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := e.driver.Reconcile(e.ctx, ref.AttemptID); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("early reconciliation: %v", err)
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || attempts[0].ExitProven() || attempts[0].Proof != nil {
		t.Fatalf("early reconciliation advanced exit handoff: %+v %v", attempts, err)
	}
	if outcome, err := e.host.Close(e.ctx); err != nil || !outcome.Released {
		t.Fatalf("original cleanup: %+v %v", outcome, err)
	}
	if connection.closes.Load() != 1 {
		t.Fatalf("protocol close calls: %d", connection.closes.Load())
	}
}

func TestCloseDeadlineAndReconcileCannotReplaceRunningProtocolCleanup(t *testing.T) {
	finish := make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	defer release()
	connection := &cleanupConnection{started: make(chan struct{}), finishClose: func(context.Context) error { <-finish; return nil }}
	e := newConfiguredEnvironment(t, false, cleanupProtocol{connection}, nil)
	if _, err := e.host.Activate(e.ctx, e.ref); err != nil {
		t.Fatal(err)
	}
	ref := originalAttempt(t, e)
	wait, cancel := context.WithTimeout(e.ctx, 100*time.Millisecond)
	defer cancel()
	type closeResult struct {
		outcome frame.CloseOutcome
		err     error
	}
	closed := make(chan closeResult, 1)
	go func() {
		outcome, err := e.host.Close(wait)
		closed <- closeResult{outcome, err}
	}()
	select {
	case <-connection.started:
	case <-e.ctx.Done():
		t.Fatal(context.Cause(e.ctx))
	}
	if _, err := e.runner.CloseAttempt(e.ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := e.driver.Reconcile(e.ctx, ref.AttemptID); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("running cleanup accepted child-only proof: %v", err)
	}
	result := <-closed
	if result.outcome.Released || len(result.outcome.Pending) == 0 || !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("unfinished cleanup lost its owner: %+v %v", result.outcome, result.err)
	}
	release()
	if outcome, err := e.host.Close(e.ctx); err != nil || !outcome.Released {
		t.Fatalf("join original cleanup: %+v %v", outcome, err)
	}
	if connection.closes.Load() != 1 {
		t.Fatal("deadline or reconciliation repeated cleanup")
	}
}

func TestReconcileAcceptsLateExitAfterOriginalLocalCleanup(t *testing.T) {
	closeDiagnostic := errors.New("original protocol close deadline")
	connection := &cleanupConnection{finishClose: func(ctx context.Context) error { <-ctx.Done(); return closeDiagnostic }}
	e := newConfiguredEnvironment(t, false, cleanupProtocol{connection}, closeDiagnostic)
	if _, err := e.host.Activate(e.ctx, e.ref); err != nil {
		t.Fatal(err)
	}
	ref := originalAttempt(t, e)
	wait, cancel := context.WithTimeout(e.ctx, 4*time.Second)
	defer cancel()
	closed, err := e.host.Close(wait)
	if closed.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) {
		t.Fatalf("missing child evidence was released: %+v %v", closed, err)
	}
	if _, err := e.runner.CloseAttempt(e.ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := e.driver.Reconcile(e.ctx, ref.AttemptID); err != nil {
		t.Fatalf("late original exit: %v", err)
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		outcome, _ := e.host.Close(e.ctx)
		if outcome.ID != closed.ID {
			t.Fatalf("reconciliation replaced original close: %+v", outcome)
		}
		if outcome.Released {
			break
		}
		select {
		case <-tick.C:
		case <-e.ctx.Done():
			t.Fatalf("original release did not complete: %+v %v", outcome, context.Cause(e.ctx))
		}
	}
	if connection.closes.Load() != 1 {
		t.Fatal("late evidence repeated protocol cleanup")
	}
}

func TestCloseDeadlineKeepsOwnershipOfLateProtocolPreparation(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	defer release()
	connection := &cleanupConnection{connect: func(ctx context.Context) error {
		close(entered)
		<-finish
		return context.Cause(ctx)
	}}
	e := newConfiguredEnvironment(t, false, cleanupProtocol{connection}, nil)
	activated := make(chan error, 1)
	go func() { _, err := e.host.Activate(e.ctx, e.ref); activated <- err }()
	select {
	case <-entered:
	case <-e.ctx.Done():
		t.Fatal(context.Cause(e.ctx))
	}
	wait, cancel := context.WithTimeout(e.ctx, 100*time.Millisecond)
	defer cancel()
	if outcome, err := e.host.Close(wait); outcome.Released || len(outcome.Pending) == 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished preparation lost its owner: %+v %v", outcome, err)
	}
	if connection.closes.Load() != 0 {
		t.Fatal("cleanup overtook original preparation")
	}
	release()
	if err := <-activated; err == nil {
		t.Fatal("retiring preparation published an export")
	}
	if outcome, err := e.host.Close(e.ctx); err != nil || !outcome.Released {
		t.Fatalf("late original preparation did not join: %+v %v", outcome, err)
	}
	if connection.closes.Load() != 1 {
		t.Fatal("late preparation skipped or repeated original protocol cleanup")
	}
}
