package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestLifetimeWorkCapacityDoesNotCancelAcceptedWork(t *testing.T) {
	ctx := testContext(t)
	lifetime, err := frame.NewLifetimeWithLimits(context.Background(), frame.LifetimeLimits{MaxWork: 2, MaxResources: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := lifetime.Close(testContext(t)); err != nil {
			t.Error(err)
		}
	})
	type result struct {
		work frame.Work
		err  error
	}
	completed := make(chan result, 16)
	for range 16 {
		go func() {
			work, err := lifetime.Enter(ctx)
			completed <- result{work, err}
		}()
	}
	var accepted []frame.Work
	for range 16 {
		result := <-completed
		if result.err == nil {
			accepted = append(accepted, result.work)
		} else if !errors.Is(result.err, frame.ErrCapacity) {
			t.Fatalf("unexpected work refusal: %v", result.err)
		}
	}
	if len(accepted) != 2 {
		t.Fatalf("concurrent admission exceeded work budget: %d", len(accepted))
	}
	for _, work := range accepted {
		if err := work.Context().Err(); err != nil {
			t.Fatalf("capacity refusal canceled accepted work: %v", err)
		}
	}
	accepted[0].Finish()
	replacement, err := lifetime.Enter(ctx)
	if err != nil {
		t.Fatalf("original completion did not release work capacity: %v", err)
	}
	replacement.Finish()
	accepted[1].Finish()
}

func TestLifetimeResourceCapacityWaitsForOriginalProof(t *testing.T) {
	ctx := testContext(t)
	lifetime, err := frame.NewLifetimeWithLimits(context.Background(), frame.LifetimeLimits{MaxWork: 1, MaxResources: 1})
	if err != nil {
		t.Fatal(err)
	}
	var originalCloses, rejectedCloses atomic.Int64
	original, err := lifetime.Own(frame.Resource{Name: "original", Close: func(context.Context) (frame.ReleaseState, error) {
		originalCloses.Add(1)
		return frame.ResourceUnconfirmed, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	rejected := frame.Resource{Name: "new", Close: func(context.Context) (frame.ReleaseState, error) {
		rejectedCloses.Add(1)
		return frame.ResourceReleased, nil
	}}
	if _, err := lifetime.Own(rejected); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("resource capacity was not enforced: %v", err)
	}
	work, err := lifetime.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := work.Own(rejected); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("work ownership bypassed the resource budget: %v", err)
	}
	if originalCloses.Load() != 0 || rejectedCloses.Load() != 0 {
		t.Fatal("capacity admission evicted or closed a resource")
	}
	if err := original.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	pending, err := lifetime.Own(frame.Resource{Name: "pending", Close: func(context.Context) (frame.ReleaseState, error) {
		originalCloses.Add(1)
		return frame.ResourceUnconfirmed, nil
	}})
	if err != nil {
		t.Fatalf("original proof did not release capacity: %v", err)
	}
	closing := lifetime.BeginClose(frame.ErrClosed)
	if _, err := work.Own(rejected); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("closing work dropped an unconfirmed resource to admit another: %v", err)
	}
	work.Finish()
	first, err := closing.Wait(ctx)
	if !errors.Is(err, frame.ErrUnconfirmedRelease) || first.Released || len(first.Pending) != 1 || first.Pending[0] != "pending" || originalCloses.Load() != 1 {
		t.Fatalf("original unknown resource was lost: %+v, %v; closes=%d", first, err, originalCloses.Load())
	}
	if err := pending.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	final, err := closing.WaitReleased(ctx)
	if err != nil || !final.Released || final.ID != first.ID || originalCloses.Load() != 1 {
		t.Fatalf("proof did not finish the original retirement: %+v, %v", final, err)
	}
}

func TestHostUseCapacityIncludesUnconfirmedRetirement(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{Limits: framehost.Limits{MaxUses: 1}})
	point, _ := framehost.NewPoint[string, string]("example.capacity", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: point.Ref()}}},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("value", point, "metadata", "original")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("provider", plugin, "main", "value", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := framehost.Select(snapshot, point, "original@1", candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	plan, err := framehost.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	config := framehost.UseConfig{Owner: owner, Scope: scope}
	use, err := host.Acquire(ctx, plan, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Acquire(ctx, plan, config); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("new Use exceeded the Host budget: %v", err)
	}
	if err := framehost.Borrow(ctx, use, selection, func(_ context.Context, values []string) error {
		if len(values) != 1 || values[0] != "original" {
			t.Fatal("capacity refusal changed the original Use")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var closes atomic.Int64
	resource, err := use.Lifetime().Own(frame.Resource{Name: "browser object", Close: func(context.Context) (frame.ReleaseState, error) {
		closes.Add(1)
		return frame.ResourceUnconfirmed, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := use.Close(ctx)
	if !errors.Is(err, frame.ErrUnconfirmedRelease) || outcome.Released || closes.Load() != 1 {
		t.Fatalf("uncertain Use retirement became final: %+v, %v", outcome, err)
	}
	if _, err := host.Acquire(ctx, plan, config); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("unknown Use released its capacity: %v", err)
	}
	if err := resource.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := use.BeginClose(frame.ErrClosed).WaitReleased(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := use.Close(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := host.Acquire(ctx, plan, config)
	if err != nil {
		t.Fatalf("settled original Use did not release capacity: %v", err)
	}
	if next.ID() == use.ID() || closes.Load() != 1 {
		t.Fatal("new admission reused or restarted the retired Use")
	}
}

func TestWatchCapacityRefusesNewStreamsWithoutEvictingOriginal(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{Limits: framehost.Limits{MaxWatches: 1, WatchBuffer: 1}})
	point, _ := framehost.NewPoint[string, string]("example.watched", "1")
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	_, original, err := host.Watch(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if snapshot, watch, err := host.Watch(ctx, point.Ref()); !errors.Is(err, frame.ErrCapacity) || snapshot != (framehost.Snapshot{}) || watch.Valid() {
		t.Fatalf("watch capacity admitted a partial stream: %v, %v, %v", snapshot, watch, err)
	}
	plugin := installNative(t, host, "watched", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: point.Ref()}}},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("value", point, "metadata", "value")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("watched", plugin, "main", "value", point.Ref())); err != nil {
		t.Fatal(err)
	}
	select {
	case change, open := <-original.Changes():
		if !open || change.Err != nil || change.Contract != point.Ref() || change.Revision == 0 {
			t.Fatalf("capacity refusal evicted the original watch: %+v, open=%v", change, open)
		}
	case <-ctx.Done():
		t.Fatal("original watch stopped observing")
	}
	original.Close()
	_, next, err := host.Watch(ctx, point.Ref())
	if err != nil {
		t.Fatalf("watch closure did not release capacity: %v", err)
	}
	next.Close()
}

func TestNegativeCoreBudgetsAreRejected(t *testing.T) {
	for _, limits := range []frame.LifetimeLimits{{MaxWork: -1}, {MaxResources: -1}} {
		if lifetime, err := frame.NewLifetimeWithLimits(t.Context(), limits); !errors.Is(err, frame.ErrInvalid) || lifetime.Valid() {
			t.Fatalf("invalid lifetime budget was accepted: %v, %v", lifetime, err)
		}
	}
	for _, config := range []framehost.Config{{Limits: framehost.Limits{MaxUses: -1}}, {Limits: framehost.Limits{MaxWatches: -1}}} {
		if host, err := framehost.New(t.Context(), config); !errors.Is(err, frame.ErrInvalid) || host.Valid() {
			t.Fatalf("invalid Host budget was accepted: %v, %v", host, err)
		}
	}
}

func TestHostResourceCapacityRefusalClosesItsOriginalUnpublishedUse(t *testing.T) {
	ctx := testContext(t)
	hostLifetime, err := frame.NewLifetimeWithLimits(context.Background(), frame.LifetimeLimits{MaxResources: 1})
	if err != nil {
		t.Fatal(err)
	}
	host := testHost(t, framehost.Config{Lifetime: hostLifetime})
	contract, _ := framehost.NewContract[string]("example.host-resource-budget", "1")
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	root := optionalExport(t, host, contract, "provider")
	binding, err := host.Activate(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(context.Background())
	scope, err := host.Admission().OpenScope(ctx, "capacity-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireExports(host, ctx, binding, framehost.UseConfig{Owner: owner, Scope: scope}); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("Host resource budget did not reject the new Use: %v", err)
	}
	outcome, err := owner.BeginClose(frame.ErrClosed).WaitReleased(ctx)
	if err != nil || !outcome.Released || len(outcome.Failures) != 0 {
		t.Fatalf("admission refusal became a fabricated cleanup failure: %+v, %v", outcome, err)
	}
}
