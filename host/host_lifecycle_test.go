package host_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestConcurrentRetirementRequestsShareOriginalWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := testHost(t, framehost.Config{})
		var cleanups atomic.Int32
		var original frame.ResourceHandle
		plugin := installNative(t, host, "worker", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				var err error
				original, err = setup.Own(frame.Resource{Name: "original execution", Close: func(context.Context) (frame.ReleaseState, error) {
					cleanups.Add(1)
					return frame.ResourceUnconfirmed, nil
				}})
				return nil, err
			},
		})
		grant, err := host.Admission().OpenScope(t.Context(), "worker-owner")
		if err != nil {
			t.Fatal(err)
		}
		running, err := host.StartUnit(t.Context(), grant, framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"})
		if err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		var requests sync.WaitGroup
		for range 8 {
			requests.Go(func() {
				if err := host.RetireInstance(t.Context(), running.ID()); err != nil {
					t.Errorf("original instance retirement failed: %v", err)
				}
			})
			requests.Go(func() {
				if err := host.Retire(t.Context(), "worker"); err != nil {
					t.Errorf("installation retirement failed: %v", err)
				}
			})
			requests.Go(func() {
				if err := running.Stop(canceled); !errors.Is(err, context.Canceled) {
					t.Errorf("canceled observer replaced the original retirement: %v", err)
				}
			})
			requests.Go(func() { host.Admission().FenceScope(grant.ID()) })
		}
		requests.Wait()
		synctest.Wait()
		if cleanups.Load() != 1 {
			t.Errorf("overlapping requests repeated or abandoned original cleanup: %d", cleanups.Load())
		}
		if err := host.WaitInstanceRetired(canceled, running.ID()); !errors.Is(err, context.Canceled) {
			t.Errorf("retirement requests invented exit proof: %v", err)
		}
		if err := original.ConfirmReleased(nil); err != nil {
			t.Fatal(err)
		}
		if err := host.WaitInstanceRetired(t.Context(), running.ID()); err != nil || cleanups.Load() != 1 {
			t.Fatalf("late proof did not complete one original retirement: cleanups=%d error=%v", cleanups.Load(), err)
		}
	})
}

func TestHostCloseJoinsOriginalChildRetirement(t *testing.T) {
	ctx := testContext(t)
	lifetime := frame.NewLifetime(context.Background())
	canceling, resume := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(resume) })
	defer unblock()
	if _, err := lifetime.Own(frame.Resource{
		Name: "original cancellation",
		Cancel: func(error) {
			close(canceling)
			<-resume
		},
		Close: func(context.Context) (frame.ReleaseState, error) { return frame.ResourceReleased, nil },
	}); err != nil {
		t.Fatal(err)
	}
	host := testHost(t, framehost.Config{Lifetime: lifetime})
	_, watch, err := host.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	cause := errors.New("original application stopped")
	lifetime.Cancel(cause)
	wait, cancel := context.WithCancel(ctx)
	cancel()
	if outcome, err := host.Close(wait); outcome.Released || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observer replaced the original close: %+v, %v", outcome, err)
	}
	select {
	case <-canceling:
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	unblock()
	if outcome, err := host.Close(ctx); !outcome.Released || err != nil {
		t.Fatalf("original child retirement did not complete: %+v, %v", outcome, err)
	}
	if err := watch.Err(); !errors.Is(err, cause) {
		t.Fatalf("host originated a second child close reason: %v", err)
	}
}

func TestHostAdmissionEndsWithItsOriginalLifetime(t *testing.T) {
	for _, closeParent := range []bool{false, true} {
		name := "explicit lifetime close"
		if closeParent {
			name = "parent cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			parent, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
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
			cause := frame.ErrClosed
			if closeParent {
				cause = errors.New("original application stopped")
				cancel(cause)
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
				if ok {
					t.Errorf("watch remained open after owner release: %+v", change)
				}
				if err := watch.Err(); !errors.Is(err, cause) {
					t.Errorf("watch lost its owner close: %v", err)
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
