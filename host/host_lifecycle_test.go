package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
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
			host := testHost(t, framehost.Config{Lifetime: lifetime})
			point, err := framehost.NewPoint[string, string]("example.host-lifetime", "1")
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
				if next.Valid() {
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
	host := testHost(t, framehost.Config{Lifetime: lifetime, Limits: framehost.Limits{MaxWatches: 2}})
	_, watch, err := host.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, extra, err := host.Watch(ctx); !errors.Is(err, frame.ErrCapacity) || snapshot != (framehost.Snapshot{}) || extra.Valid() {
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
	if host, err := framehost.New(ctx, framehost.Config{Lifetime: lifetime}); host.Valid() || !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("retired lifetime created a host: %v, %v", host, err)
	}
}

func TestRetirementRemainsAvailableDuringRecovery(t *testing.T) {
	for _, installation := range []bool{false, true} {
		name := "instance"
		if installation {
			name = "installation"
		}
		t.Run(name, func(t *testing.T) {
			host := testHost(t, framehost.Config{})
			var cleanups atomic.Int32
			plugin := installNative(t, host, "retiring", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
					_, err := setup.Cleanup("original resource", func() error { cleanups.Add(1); return nil })
					return nil, err
				},
			})
			root := framehost.UnitRef{InstallationID: "retiring", Release: plugin.Revision(), UnitID: "main"}
			if _, err := host.ActivateUnit(t.Context(), "", root); err != nil {
				t.Fatal(err)
			}
			original := host.Observations()[0].ID
			host.Admission().RequireRecovery(errors.New("storage requires reconciliation"))
			var err error
			if installation {
				err = host.Retire(t.Context(), root.InstallationID)
			} else {
				err = host.RetireInstance(t.Context(), original)
			}
			if err != nil {
				t.Fatalf("recovery blocked original retirement: %v", err)
			}
			if err := host.WaitInstanceRetired(testContext(t), original); err != nil {
				t.Fatal(err)
			}
			if cleanups.Load() != 1 {
				t.Fatalf("original cleanup count = %d", cleanups.Load())
			}
			if _, err := host.ActivateUnit(t.Context(), "", root); !errors.Is(err, framehost.ErrRecoveryRequired) {
				t.Fatalf("retirement reopened new admission: %v", err)
			}
		})
	}
}
