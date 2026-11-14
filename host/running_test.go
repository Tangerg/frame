package host_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestStartDoesNotReserveUseResources(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		maxResources int
		occupiedUse  bool
	}{
		{name: "only the instance resource fits", maxResources: 1},
		{name: "use capacity is occupied", occupiedUse: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := testContext(t)
			lifetime, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxResources: scenario.maxResources})
			if err != nil {
				t.Fatal(err)
			}
			host := testHost(t, framehost.Config{Lifetime: lifetime, Limits: framehost.Limits{MaxUses: 1}})
			owner, grant := ownerAndScope(t, host)
			if scenario.occupiedUse {
				plugin := installNative(t, host, "borrowed", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
					Setup:       func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil },
				})
				root := framehost.UnitRef{InstallationID: "borrowed", Release: plugin.Revision(), UnitID: "main"}
				if _, err := host.AcquireUnit(ctx, root, framehost.UseConfig{Owner: owner, Scope: grant}); err != nil {
					t.Fatal(err)
				}
			}
			var cleanups atomic.Int32
			plugin := installNative(t, host, "worker", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
					_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
					return nil, err
				},
			})
			root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
			first, err := host.StartUnit(ctx, grant, root)
			if err != nil {
				t.Fatalf("start claim required an unrelated Use reservation: %v", err)
			}
			second, err := host.StartUnit(ctx, grant, root)
			if err != nil {
				t.Fatal(err)
			}
			if first.ID() != second.ID() {
				t.Fatal("independent claims did not share the original instance")
			}
			if err := first.Stop(ctx); err != nil || cleanups.Load() != 0 {
				t.Fatalf("one claim retired another client's instance: cleanups=%d error=%v", cleanups.Load(), err)
			}
			if err := second.Stop(ctx); err != nil || cleanups.Load() != 1 {
				t.Fatalf("final claim did not retire the original instance once: cleanups=%d error=%v", cleanups.Load(), err)
			}
		})
	}
}

func TestStartRefusesGrantFencedDuringPreparation(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	grant, err := host.Admission().OpenScope(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			if _, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil }); err != nil {
				return nil, err
			}
			return nil, setup.OnActivate(func(context.Context) error {
				host.Admission().FenceScope(grant.ID())
				return nil
			})
		},
	})
	root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	if running, err := host.StartUnit(ctx, grant, root); running.Valid() || err == nil {
		t.Fatalf("preparation replaced the original fenced grant: running=%v error=%v", running, err)
	}
	if err := host.WaitRetired(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if cleanups.Load() != 1 {
		t.Fatalf("refused start did not retire its original preparation once: %d", cleanups.Load())
	}
}

func TestRunningStopJoinsOriginalRetirementAfterCanceledWait(t *testing.T) {
	host := testHost(t, framehost.Config{})
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error {
				cleanups.Add(1)
				close(entered)
				<-release
				return nil
			})
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(t.Context(), "worker-scope")
	if err != nil {
		t.Fatal(err)
	}
	running, err := host.StartUnit(t.Context(), grant, framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := running.Stop(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stop wait: %v", err)
	}
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if err := running.Stop(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation replaced the original retirement: %v", err)
	}
	finished := make(chan error, 2)
	for range cap(finished) {
		go func() { finished <- running.Stop(t.Context()) }()
	}
	unblock()
	for range cap(finished) {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	if err := running.Stop(canceled); err != nil || cleanups.Load() != 1 {
		t.Fatalf("completed retirement changed: cleanups=%d error=%v", cleanups.Load(), err)
	}
}
