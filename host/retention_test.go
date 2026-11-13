package host

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"weak"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type retainedHostPayload struct{ bytes [1 << 20]byte }

func awaitOwnerCollected[T any](t *testing.T, value weak.Pointer[T]) {
	t.Helper()
	for range 100 {
		runtime.GC()
		if value.Value() == nil {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("terminal handle retained its former resource owner")
}

func closedLifetime(t *testing.T) (frame.Close, weak.Pointer[retainedHostPayload]) {
	t.Helper()
	value := new(retainedHostPayload)
	value.bytes[0] = 1
	type key struct{}
	owner := frame.NewLifetime(context.WithValue(context.Background(), key{}, value))
	exit := owner.BeginClose(frame.ErrClosed)
	if outcome, err := exit.WaitReleased(t.Context()); err != nil || !outcome.Released {
		t.Fatalf("close: %+v, %v", outcome, err)
	}
	return exit, weak.Make(value)
}

func TestTerminalExitDoesNotRetainOriginalContext(t *testing.T) {
	exit, value := closedLifetime(t)
	awaitOwnerCollected(t, value)
	if outcome, err := exit.Wait(t.Context()); err != nil || !outcome.Released {
		t.Fatalf("retained observation: %+v, %v", outcome, err)
	}
	runtime.KeepAlive(exit)
}

func closedUse(t *testing.T) (*Use, weak.Pointer[hostState], weak.Pointer[retainedHostPayload]) {
	t.Helper()
	ctx := t.Context()
	host, err := New(ctx, Config{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewContract[*retainedHostPayload]("retention.value", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	value := new(retainedHostPayload)
	value.bytes[0] = 1
	plugin, err := Define(Definition{Revision: framevalue.Hash([]byte("retention")), Units: []Unit{{
		Declaration: UnitDeclaration{ID: "value", Profile: NativeGo, Exports: []ExportDeclaration{{Name: "value", Contract: service.Ref()}}},
		Setup: func(context.Context, *Setup) ([]ExportValue, error) {
			return []ExportValue{Provide("value", service, value)}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, InstallationConfig{ID: "retention", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	source := ExportRef{InstallationID: "retention", Release: plugin.Revision(), UnitID: "value", Export: "value", Contract: service.Ref()}
	binding, err := host.Activate(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(ctx)
	scope, err := host.Admission().OpenScope(ctx, "retention")
	if err != nil {
		t.Fatal(err)
	}
	use, err := acquireRetentionUse(host, ctx, binding, UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := BorrowExport(ctx, use, source, func(_ context.Context, actual *retainedHostPayload) error {
		if actual != value {
			t.Fatal("borrow did not preserve the original native object")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	copied := *use
	if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
		t.Fatalf("use close: %+v, %v", outcome, err)
	}
	if _, err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if outcome, err := host.Close(ctx); err != nil || !outcome.Released {
		t.Fatalf("host close: %+v, %v", outcome, err)
	}
	return &copied, weak.Make(host.hostState), weak.Make(value)
}

func TestTerminalUseDoesNotRetainHostOrNativeObject(t *testing.T) {
	use, host, value := closedUse(t)
	awaitOwnerCollected(t, host)
	awaitOwnerCollected(t, value)
	if outcome, err := use.Close(t.Context()); err != nil || !outcome.Released {
		t.Fatalf("original exit: %+v, %v", outcome, err)
	}
	if use.ID() == "" || len(use.Binding().Roots) != 1 {
		t.Fatal("terminal use lost its immutable identity")
	}
	if err := BorrowExport(t.Context(), use, use.Sources()[0], func(context.Context, *retainedHostPayload) error {
		t.Fatal("terminal use dispatched work")
		return nil
	}); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("terminal borrow: %v", err)
	}
	runtime.KeepAlive(use)
}

func stoppedRunningUnit(t *testing.T) (*RunningUnit, weak.Pointer[hostState], weak.Pointer[retainedHostPayload]) {
	t.Helper()
	host, err := New(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	value := new(retainedHostPayload)
	value.bytes[0] = 1
	unit, err := DefineUnit(UnitSpec{ID: "worker", Prepare: func(*Setup) error {
		if value.bytes[0] != 1 {
			return errors.New("retained payload changed")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := Define(Definition{Revision: framevalue.Hash([]byte("running retention")), Units: []Unit{unit}})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := host.Install(t.Context(), InstallationConfig{ID: "worker", Plugin: plugin, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := host.Admission().OpenScope(t.Context(), "running-retention")
	if err != nil {
		t.Fatal(err)
	}
	running, err := host.StartUnit(t.Context(), grant, installed.Unit("worker"))
	if err != nil {
		t.Fatal(err)
	}
	if err := running.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if outcome, err := host.Close(t.Context()); err != nil || !outcome.Released {
		t.Fatalf("host close: %+v, %v", outcome, err)
	}
	return running, weak.Make(host.hostState), weak.Make(value)
}

func TestStoppedRunningUnitDoesNotRetainHostOrPlugin(t *testing.T) {
	running, host, value := stoppedRunningUnit(t)
	id := running.ID()
	awaitOwnerCollected(t, host)
	awaitOwnerCollected(t, value)
	if err := running.Stop(t.Context()); err != nil {
		t.Fatalf("original retirement: %v", err)
	}
	if id == "" || running.ID() != id {
		t.Fatal("stopped handle lost its original instance identity")
	}
	runtime.KeepAlive(running)
}

func acquireRetentionUse(host Host, ctx context.Context, binding BindingSnapshot, config UseConfig) (*Use, error) {
	plan, err := host.PlanWith(ctx, config.Realm, nil, binding.Roots...)
	if err != nil {
		return nil, err
	}
	return host.Acquire(ctx, plan, config)
}
