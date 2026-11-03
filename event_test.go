package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestNativeEventRouteIsOwnedByUsesAndCallbackCanReenter(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	event, _ := frame.NewEvent[*int]("example.progress", "1")
	if err := host.Register(event); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "event", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "source", Contract: event.Ref()}, {Name: "receiver", Contract: event.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return []frame.ExportValue{{Name: "source", Contract: event.Ref(), Durable: true}, {Name: "receiver", Contract: event.Ref(), Durable: true}}, nil
	}})
	sender := source("event", plugin, "main", "source", event.Ref())
	receiver := source("event", plugin, "main", "receiver", event.Ref())
	binding, err := host.Activate(ctx, sender, receiver)
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	use, err := acquireExports(host, ctx, binding, frame.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	route, err := host.NewEventRoute(ctx, use, use, sender, receiver)
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := frame.NewEvent[string]("example.progress", "1")
	if err := frame.DeliverEvent(ctx, host, route, wrong, "different type", func(context.Context, string) error {
		t.Fatal("unregistered payload reached native receiver")
		return nil
	}); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("mismatched local event type: %v", err)
	}
	value := 7
	if err := frame.DeliverEvent(ctx, host, route, event, &value, func(callbackCtx context.Context, payload *int) error {
		if payload != &value {
			t.Fatal("native payload was serialized")
		}
		_, err := host.Snapshot(callbackCtx, event.Ref())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := use.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.ValidateEventRoute(ctx, route); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("route survived its original use: %v", err)
	}
}

func TestWatchOverflowRequiresResnapshot(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{Limits: frame.HostLimits{WatchBuffer: 1}})
	point, _ := frame.NewPoint[string, string]("example.watch", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	var setup *frame.Setup
	plugin := installNative(t, host, "watch", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "initial", Contract: point.Ref()}}}, Setup: func(_ context.Context, s *frame.Setup) ([]frame.ExportValue, error) {
		setup = s
		return []frame.ExportValue{frame.Contribute("initial", point, "initial", "initial")}, nil
	}})
	if _, err := host.Activate(ctx, source("watch", plugin, "main", "initial", point.Ref())); err != nil {
		t.Fatal(err)
	}
	_, watch, err := host.Watch(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := host.Publish(ctx, name, preparePublication(t, host, setup.InstanceID(), frame.Contribute(name, point, name, name))); err != nil {
			t.Fatal(err)
		}
	}
	change, open := <-watch.Changes()
	if !open || !errors.Is(change.Err, frame.ErrResyncRequired) {
		t.Fatalf("revision gap silently dropped: %+v", change)
	}
	if _, open := <-watch.Changes(); open {
		t.Fatal("overflowed watch stayed open")
	}
	watch.Close()
}
