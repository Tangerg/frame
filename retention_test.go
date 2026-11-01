package frame_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"weak"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type retainedPayload struct{ bytes [1 << 20]byte }

func awaitCollected[T any](t *testing.T, value weak.Pointer[T]) {
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

func closedLifetime(t *testing.T) (*frame.Close, weak.Pointer[retainedPayload]) {
	t.Helper()
	value := new(retainedPayload)
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
	awaitCollected(t, value)
	if outcome, err := exit.Wait(t.Context()); err != nil || !outcome.Released {
		t.Fatalf("retained observation: %+v, %v", outcome, err)
	}
	runtime.KeepAlive(exit)
}

func closedUse(t *testing.T) (*frame.Use, weak.Pointer[frame.Host], weak.Pointer[retainedPayload]) {
	t.Helper()
	ctx := t.Context()
	host, err := frame.NewHost(ctx, frame.HostConfig{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := frame.NewContract[*retainedPayload]("retention.value", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	value := new(retainedPayload)
	value.bytes[0] = 1
	plugin, err := frame.Define(frame.Definition{Revision: framevalue.Hash([]byte("retention")), Units: []frame.Unit{{
		Declaration: frame.UnitDeclaration{ID: "value", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "value", Contract: service.Ref()}}},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Provide("value", service, value)}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "retention", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	source := frame.ExportRef{InstallationID: "retention", Release: plugin.Revision(), UnitID: "value", Export: "value", Contract: service.Ref()}
	binding, err := host.Activate(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(ctx)
	scope, err := host.Admission().OpenScope(ctx, "retention")
	if err != nil {
		t.Fatal(err)
	}
	use, err := acquireExports(host, ctx, binding, frame.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.BorrowExport(ctx, use, source, func(_ context.Context, actual *retainedPayload) error {
		if actual != value {
			t.Fatal("borrow did not preserve the original native object")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
		t.Fatalf("use close: %+v, %v", outcome, err)
	}
	if _, err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if outcome, err := host.Close(ctx); err != nil || !outcome.Released {
		t.Fatalf("host close: %+v, %v", outcome, err)
	}
	return use, weak.Make(host), weak.Make(value)
}

func TestTerminalUseDoesNotRetainHostOrNativeObject(t *testing.T) {
	use, host, value := closedUse(t)
	awaitCollected(t, host)
	awaitCollected(t, value)
	if outcome, err := use.Close(t.Context()); err != nil || !outcome.Released {
		t.Fatalf("original exit: %+v, %v", outcome, err)
	}
	if use.ID() == "" || len(use.Binding().Roots) != 1 {
		t.Fatal("terminal use lost its immutable identity")
	}
	if err := frame.BorrowExport(t.Context(), use, use.Sources()[0], func(context.Context, *retainedPayload) error {
		t.Fatal("terminal use dispatched work")
		return nil
	}); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("terminal borrow: %v", err)
	}
	runtime.KeepAlive(use)
}
