package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestNativeEventRouteIsOwnedByUsesAndCallbackCanReenter(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	event, _ := framehost.NewEvent[*int]("example.progress", "1")
	if err := host.Register(event); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "event", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "source", Contract: event.Ref()}, {Name: "receiver", Contract: event.Ref()}}}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
		return []framehost.ExportValue{{Name: "source", Contract: event.Ref()}, {Name: "receiver", Contract: event.Ref()}}, nil
	}})
	sender := source("event", plugin, "main", "source", event.Ref())
	receiver := source("event", plugin, "main", "receiver", event.Ref())
	binding, err := host.Activate(ctx, sender, receiver)
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	use, err := acquireExports(host, ctx, binding, framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	route, err := host.NewEventRoute(ctx, use, use, sender, receiver)
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := framehost.NewEvent[string]("example.progress", "1")
	if err := framehost.DeliverEvent(ctx, host, route, wrong, "different type", func(context.Context, string) error {
		t.Fatal("unregistered payload reached native receiver")
		return nil
	}); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("mismatched local event type: %v", err)
	}
	value := 7
	if err := framehost.DeliverEvent(ctx, host, route, event, &value, func(callbackCtx context.Context, payload *int) error {
		if payload != &value {
			t.Fatal("native payload was serialized")
		}
		_, err := host.Snapshot(callbackCtx, event.Ref())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	work, err := use.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer work.Finish()
	use.BeginClose(frame.ErrClosed)
	if err := host.ValidateEventRoute(ctx, route); !errors.Is(err, frame.ErrClosed) {
		t.Errorf("route validation outlived its original use admission: %v", err)
	}
	work.Finish()
	if _, err := use.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.ValidateEventRoute(ctx, route); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("route survived its original use: %v", err)
	}
}

func TestWatchOverflowRequiresResnapshot(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{Limits: framehost.Limits{WatchBuffer: 1}})
	point, _ := framehost.NewPoint[string, string]("example.watch", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	var setup framehost.Setup
	plugin := installNative(t, host, "watch", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "initial", Contract: point.Ref()}}}, Setup: func(_ context.Context, s framehost.Setup) ([]framehost.ExportValue, error) {
		setup = s
		return []framehost.ExportValue{framehost.Contribute("initial", point, "initial", "initial")}, nil
	}})
	if _, err := host.Activate(ctx, source("watch", plugin, "main", "initial", point.Ref())); err != nil {
		t.Fatal(err)
	}
	_, watch, err := host.Watch(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := host.Publish(ctx, name, preparePublication(t, host, setup.InstanceID(), framehost.Contribute(name, point, name, name))); err != nil {
			t.Fatal(err)
		}
	}
	change, open := <-watch.Changes()
	if !open || !errors.Is(change.Err, framehost.ErrResyncRequired) {
		t.Fatalf("revision gap silently dropped: %+v", change)
	}
	if _, open := <-watch.Changes(); open {
		t.Fatal("overflowed watch stayed open")
	}
	watch.Close()
}
