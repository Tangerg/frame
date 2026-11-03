package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestHostAdmissionEndsWithItsOriginalLifetime(t *testing.T) {
	for _, closeParent := range []bool{false, true} {
		name := "explicit lifetime close"
		if closeParent {
			name = "parent cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			lifetime := frame.NewLifetime(parent)
			host := testHost(t, frame.HostConfig{Lifetime: lifetime})
			point, err := frame.NewPoint[string, string]("example.host-lifetime", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(point); err != nil {
				t.Fatal(err)
			}
			_, watch, err := host.Watch(ctx, point.Ref())
			if err != nil {
				t.Fatal(err)
			}
			defer watch.Close()
			if closeParent {
				cancel()
				select {
				case <-lifetime.Context().Done():
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if _, err := lifetime.BeginClose(frame.ErrClosed).WaitReleased(ctx); err != nil {
				t.Fatal(err)
			}
			if err := host.Register(point); !errors.Is(err, frame.ErrClosed) {
				t.Errorf("closed lifetime admitted a declaration: %v", err)
			}
			if _, err := host.Plan(ctx); !errors.Is(err, frame.ErrClosed) {
				t.Errorf("closed lifetime admitted a plan: %v", err)
			}
			if _, err := host.Activate(ctx); !errors.Is(err, frame.ErrClosed) {
				t.Errorf("closed lifetime admitted activation: %v", err)
			}
			if _, err := host.Snapshot(ctx, point.Ref()); !errors.Is(err, frame.ErrClosed) {
				t.Errorf("closed lifetime admitted a snapshot: %v", err)
			}
			if _, next, err := host.Watch(ctx, point.Ref()); !errors.Is(err, frame.ErrClosed) {
				if next != nil {
					next.Close()
				}
				t.Errorf("closed lifetime admitted a watch: %v", err)
			}
			select {
			case change, ok := <-watch.Changes():
				if !ok || !errors.Is(change.Err, frame.ErrClosed) {
					t.Errorf("watch lost its owner close: %+v, open=%v", change, ok)
				}
				if _, ok := <-watch.Changes(); ok {
					t.Error("watch remained open after owner release")
				}
			default:
				t.Error("released lifetime retained an open watch")
			}
		})
	}
}

func TestHostWatchResourceBudgetTracksActualSubscriptions(t *testing.T) {
	ctx := testContext(t)
	lifetime, err := frame.NewLifetimeWithLimits(context.Background(), frame.LifetimeLimits{MaxResources: 1})
	if err != nil {
		t.Fatal(err)
	}
	host := testHost(t, frame.HostConfig{Lifetime: lifetime, Limits: frame.HostLimits{MaxWatches: 2}})
	_, watch, err := host.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, extra, err := host.Watch(ctx); !errors.Is(err, frame.ErrCapacity) || snapshot != nil || extra != nil {
		t.Fatalf("watch escaped its lifetime capacity: %v, %v, %v", snapshot, extra, err)
	}
	watch.Close()
	_, replacement, err := host.Watch(ctx)
	if err != nil {
		t.Fatalf("closed watch retained its resource capacity: %v", err)
	}
	replacement.Close()
}

func TestNewHostRefusesRetiredLifetime(t *testing.T) {
	ctx := testContext(t)
	lifetime := frame.NewLifetime(context.Background())
	if _, err := lifetime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if host, err := frame.NewHost(ctx, frame.HostConfig{Lifetime: lifetime}); host != nil || !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("retired lifetime created a host: %v, %v", host, err)
	}
}
