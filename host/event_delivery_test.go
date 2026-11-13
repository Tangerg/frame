package host_test

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

type eventDeliveryFixture struct {
	host             framehost.Host
	hostLifetime     frame.Lifetime
	instanceLifetime frame.Lifetime
	event            framehost.Event[string]
	source, receiver framehost.ExportRef
	binding          framehost.BindingSnapshot
	owner            frame.Lifetime
	scope            framehost.ScopeGrant
}

func newEventDeliveryFixture(t *testing.T) eventDeliveryFixture {
	t.Helper()
	ctx := testContext(t)
	hostLifetime := frame.NewLifetime(context.Background())
	host := testHost(t, framehost.Config{Lifetime: hostLifetime})
	event, err := framehost.NewEvent[string]("delivery.original", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(event); err != nil {
		t.Fatal(err)
	}
	var instanceLifetime frame.Lifetime
	plugin := installNative(t, host, "event", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "source", Contract: event.Ref()}, {Name: "receiver", Contract: event.Ref()}}},
		Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
			instanceLifetime = setup.Lifetime()
			return []framehost.ExportValue{{Name: "source", Contract: event.Ref()}, {Name: "receiver", Contract: event.Ref()}}, nil
		},
	})
	sender := source("event", plugin, "main", "source", event.Ref())
	receiver := source("event", plugin, "main", "receiver", event.Ref())
	binding, err := host.Activate(ctx, sender, receiver)
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	return eventDeliveryFixture{host: host, hostLifetime: hostLifetime, instanceLifetime: instanceLifetime, event: event, source: sender, receiver: receiver, binding: binding, owner: owner, scope: scope}
}

func (f eventDeliveryFixture) acquire(t *testing.T) *framehost.Use {
	t.Helper()
	use, err := acquireExports(f.host, testContext(t), f.binding, framehost.UseConfig{Owner: f.owner, Scope: f.scope})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if outcome, err := use.Close(testContext(t)); !outcome.Released || err != nil {
			t.Errorf("original Use close: %+v, %v", outcome, err)
		}
	})
	return use
}

func occupyUseWork(t *testing.T, use *framehost.Use) []*frame.Work {
	t.Helper()
	ctx := testContext(t)
	var held []*frame.Work
	t.Cleanup(func() {
		for _, work := range held {
			work.Finish()
		}
	})
	for {
		work, err := use.Enter(ctx)
		if errors.Is(err, frame.ErrCapacity) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, work)
	}
	return held
}

func TestEventDeliveryReservesOneIntervalPerDistinctUse(t *testing.T) {
	ctx := testContext(t)
	fixture := newEventDeliveryFixture(t)
	use := fixture.acquire(t)
	copied := *use
	route, err := fixture.host.NewEventRoute(ctx, use, &copied, fixture.source, fixture.receiver)
	if err != nil {
		t.Fatal(err)
	}
	held := occupyUseWork(t, use)
	if len(held) == 0 {
		t.Fatal("Use admitted no original work intervals")
	}
	held[len(held)-1].Finish()
	var calls int
	deliver := func() error {
		return framehost.DeliverEvent(ctx, fixture.host, route, fixture.event, "original", func(_ context.Context, payload string) error {
			if payload != "original" {
				t.Fatal("delivery changed the original payload")
			}
			calls++
			return nil
		})
	}
	if err := deliver(); err != nil {
		t.Fatalf("one participant needed more than its remaining interval: %v", err)
	}
	if err := deliver(); err != nil || calls != 2 {
		t.Fatalf("delivery retained capacity or repeated its callback: calls=%d error=%v", calls, err)
	}
	remaining, err := use.Enter(ctx)
	if err != nil {
		t.Fatalf("delivery leaked its original interval: %v", err)
	}
	defer remaining.Finish()
	if extra, err := use.Enter(ctx); !errors.Is(err, frame.ErrCapacity) {
		if extra != nil {
			extra.Finish()
		}
		t.Fatalf("delivery released another caller's interval: %v", err)
	}
}

func TestEventRouteReservesOneResourcePerDistinctUse(t *testing.T) {
	ctx := testContext(t)
	fixture := newEventDeliveryFixture(t)
	use := fixture.acquire(t)
	lifetime := use.Lifetime()
	var held []*frame.ResourceHandle
	t.Cleanup(func() {
		for _, resource := range held {
			if err := resource.ConfirmReleased(nil); err != nil {
				t.Error(err)
			}
		}
	})
	for {
		resource, err := lifetime.Own(frame.Resource{Name: "original caller", Close: func(context.Context) (frame.ReleaseState, error) { return frame.ResourceReleased, nil }})
		if errors.Is(err, frame.ErrCapacity) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, resource)
	}
	if len(held) == 0 {
		t.Fatal("Use admitted no original resources")
	}
	if err := held[len(held)-1].ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	copied := *use
	route, err := fixture.host.NewEventRoute(ctx, use, &copied, fixture.source, fixture.receiver)
	if err != nil {
		t.Fatalf("one participant needed more than its remaining resource: %v", err)
	}
	if _, err := fixture.host.NewEventRoute(ctx, use, &copied, fixture.source, fixture.receiver); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("route released another caller's resource: %v", err)
	}
	if err := fixture.host.ValidateEventRoute(ctx, route); err != nil {
		t.Fatalf("refused route replaced its original predecessor: %v", err)
	}
}

func TestEventDeliveryReadsOriginalPermission(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, authority := range []string{"host", "owner", "scope", "instance"} {
		t.Run(authority, func(t *testing.T) {
			ctx := testContext(t)
			fixture := newEventDeliveryFixture(t)
			use := fixture.acquire(t)
			route, err := fixture.host.NewEventRoute(ctx, use, use, fixture.source, fixture.receiver)
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("original authority cancelled")
			err = framehost.DeliverEvent(ctx, fixture.host, route, fixture.event, "payload", func(context.Context, string) error {
				switch authority {
				case "host":
					fixture.hostLifetime.Cancel(cause)
				case "owner":
					fixture.owner.Cancel(cause)
				case "scope":
					fixture.host.Admission().FenceScope(fixture.scope.ID())
				case "instance":
					fixture.instanceLifetime.Cancel(cause)
				}
				return nil
			})
			denied := errors.Is(err, frame.ErrClosed)
			if authority == "scope" {
				denied = denied || errors.Is(err, framehost.ErrFenced)
			} else {
				denied = denied || errors.Is(err, context.Canceled) && errors.Is(err, cause)
			}
			if !denied {
				t.Fatalf("pending notification overrode original permission: %v", err)
			}
		})
	}
}

func TestEventDeliveryRefusalReleasesOnlyItsOwnIntervals(t *testing.T) {
	ctx := testContext(t)
	fixture := newEventDeliveryFixture(t)
	sender, receiver := fixture.acquire(t), fixture.acquire(t)
	route, err := fixture.host.NewEventRoute(ctx, sender, receiver, fixture.source, fixture.receiver)
	if err != nil {
		t.Fatal(err)
	}
	sourceWork, receiverWork := occupyUseWork(t, sender), occupyUseWork(t, receiver)
	if len(sourceWork) == 0 || len(receiverWork) == 0 {
		t.Fatal("Use admitted no original work intervals")
	}
	sourceWork[len(sourceWork)-1].Finish()
	var calls int
	deliver := func() error {
		return framehost.DeliverEvent(ctx, fixture.host, route, fixture.event, "payload", func(context.Context, string) error {
			calls++
			return nil
		})
	}
	if err := deliver(); !errors.Is(err, frame.ErrCapacity) || calls != 0 {
		t.Fatalf("refused receiver reached application code: calls=%d error=%v", calls, err)
	}
	remaining, err := sender.Enter(ctx)
	if err != nil {
		t.Fatalf("refused delivery retained its sender interval: %v", err)
	}
	remaining.Finish()
	if extra, err := receiver.Enter(ctx); !errors.Is(err, frame.ErrCapacity) {
		if extra != nil {
			extra.Finish()
		}
		t.Fatalf("refused delivery released another receiver interval: %v", err)
	}
	receiverWork[len(receiverWork)-1].Finish()
	if err := deliver(); err != nil || calls != 1 {
		t.Fatalf("original route could not deliver after capacity returned: calls=%d error=%v", calls, err)
	}
}

func TestEventDeliveryReadsOriginalParticipantCancellation(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, scenario := range []struct {
		name       string
		sourceEnds bool
		failure    bool
	}{
		{name: "source", sourceEnds: true},
		{name: "receiver"},
		{name: "source with callback failure", sourceEnds: true, failure: true},
		{name: "receiver with callback failure", failure: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := testContext(t)
			fixture := newEventDeliveryFixture(t)
			sender, receiver := fixture.acquire(t), fixture.acquire(t)
			route, err := fixture.host.NewEventRoute(ctx, sender, receiver, fixture.source, fixture.receiver)
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("original participant stopped")
			var callbackFailure error
			if scenario.failure {
				callbackFailure = errors.New("original callback failure")
			}
			var calls int
			err = framehost.DeliverEvent(ctx, fixture.host, route, fixture.event, "payload", func(context.Context, string) error {
				calls++
				participant := receiver
				if scenario.sourceEnds {
					participant = sender
				}
				participant.BeginClose(cause)
				return callbackFailure
			})
			if calls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || callbackFailure != nil && !errors.Is(err, callbackFailure) {
				t.Fatalf("queued notification replaced the original delivery conclusion: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestInterruptedEventDeliveryReleasesItsOriginalIntervals(t *testing.T) {
	cause := errors.New("original event callback interrupted")
	for _, interruption := range []struct {
		name       string
		run        func()
		panicValue any
	}{
		{name: "panic", run: func() { panic(cause) }, panicValue: cause},
		{name: "goexit", run: runtime.Goexit},
	} {
		t.Run(interruption.name, func(t *testing.T) {
			ctx := testContext(t)
			fixture := newEventDeliveryFixture(t)
			sender, receiver := fixture.acquire(t), fixture.acquire(t)
			route, err := fixture.host.NewEventRoute(ctx, sender, receiver, fixture.source, fixture.receiver)
			if err != nil {
				t.Fatal(err)
			}
			completed := make(chan any, 1)
			go func() {
				defer func() { completed <- recover() }()
				_ = framehost.DeliverEvent(ctx, fixture.host, route, fixture.event, "payload", func(context.Context, string) error {
					interruption.run()
					return nil
				})
				t.Error("interrupted callback returned normally")
			}()
			select {
			case recovered := <-completed:
				if recovered != interruption.panicValue {
					t.Fatalf("original callback interruption changed: %v", recovered)
				}
			case <-ctx.Done():
				t.Fatal("original callback did not finish")
			}
			for _, use := range []*framehost.Use{sender, receiver} {
				if outcome, err := use.Close(ctx); !outcome.Released || err != nil {
					t.Fatalf("interrupted delivery retained its original Use: %+v, %v", outcome, err)
				}
			}
		})
	}
}
