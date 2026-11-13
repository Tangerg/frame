package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestEventRouteHandleCopiesRetainOnlyTheirOriginalAuthority(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{Limits: framehost.Limits{MaxEventRoutes: 1}})
	event, err := framehost.NewEvent[string]("test.event-handle", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(event); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "event", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "source", Contract: event.Ref()}, {Name: "receiver", Contract: event.Ref()}}}, Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
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
	originalUse := *use
	*use = framehost.Use{}
	use = &originalUse
	original := *route
	*route = framehost.EventRoute{}
	if err := host.ValidateEventRoute(ctx, route); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("zero handle retained authority: %v", err)
	}
	if original.ID() == "" || original.Source() != sender || original.Receiver() != receiver {
		t.Fatal("copy lost its original identity")
	}
	if err := host.ValidateEventRoute(ctx, &original); err != nil {
		t.Fatalf("copy lost its original authority: %v", err)
	}
	var delivered string
	if err := framehost.DeliverEvent(ctx, host, &original, event, "payload", func(_ context.Context, value string) error {
		delivered = value
		return nil
	}); err != nil || delivered != "payload" {
		t.Fatalf("copy could not deliver: %q, %v", delivered, err)
	}
	if _, err := host.NewEventRoute(ctx, use, use, sender, receiver); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("copy created a second capacity owner: %v", err)
	}
	if result, err := use.Close(ctx); err != nil || !result.Released {
		t.Fatalf("original Use did not release: %+v, %v", result, err)
	}
	if err := host.ValidateEventRoute(ctx, &original); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("copy outlived its original Use: %v", err)
	}
	next, err := acquireExports(host, ctx, binding, framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.NewEventRoute(ctx, next, next, sender, receiver); err != nil {
		t.Fatalf("overwritten handle retained Host capacity: %v", err)
	}
	if result, err := next.Close(ctx); err != nil || !result.Released {
		t.Fatalf("next Use did not release: %+v, %v", result, err)
	}
}
