package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestEveryStartClaimJoinsAuthoritativeRetirement(t *testing.T) {
	for _, operation := range []struct {
		name   string
		retire func(context.Context, framehost.Host, framehost.RunningUnit, framehost.ScopeGrant) error
	}{
		{name: "instance", retire: func(ctx context.Context, host framehost.Host, running framehost.RunningUnit, _ framehost.ScopeGrant) error {
			return host.RetireInstance(ctx, running.ID())
		}},
		{name: "installation", retire: func(ctx context.Context, host framehost.Host, _ framehost.RunningUnit, _ framehost.ScopeGrant) error {
			return host.Retire(ctx, "worker")
		}},
		{name: "scope", retire: func(_ context.Context, host framehost.Host, _ framehost.RunningUnit, grant framehost.ScopeGrant) error {
			host.Admission().FenceScope(grant.ID())
			return nil
		}},
		{name: "host", retire: func(ctx context.Context, host framehost.Host, _ framehost.RunningUnit, _ framehost.ScopeGrant) error {
			wait, cancel := context.WithCancel(ctx)
			cancel()
			outcome, err := host.Close(wait)
			if outcome.Released {
				return errors.New("host supplied proof before original resource release")
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, frame.ErrUnconfirmedRelease) {
				return nil
			}
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				host := testHost(t, framehost.Config{})
				var original frame.ResourceHandle
				var cleanups atomic.Int32
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
				grant, err := host.Admission().OpenScope(t.Context(), "original-owner")
				if err != nil {
					t.Fatal(err)
				}
				root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
				first, err := host.StartUnit(t.Context(), grant, root)
				if err != nil {
					t.Fatal(err)
				}
				second, err := host.StartUnit(t.Context(), grant, root)
				if err != nil {
					t.Fatal(err)
				}
				host.Admission().RequireRecovery(errors.New("original recovery barrier"))
				if err := operation.retire(t.Context(), host, first, grant); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				canceled, cancel := context.WithCancel(t.Context())
				cancel()
				for _, claim := range []framehost.RunningUnit{first, second} {
					if err := claim.Stop(canceled); !errors.Is(err, context.Canceled) {
						t.Errorf("claim did not join the original pending retirement: %v", err)
					}
				}
				if err := original.ConfirmReleased(nil); err != nil {
					t.Fatal(err)
				}
				if err := host.WaitInstanceRetired(t.Context(), first.ID()); err != nil {
					t.Fatal(err)
				}
				for _, claim := range []framehost.RunningUnit{first, second} {
					if err := claim.Stop(canceled); err != nil {
						t.Errorf("completed retirement lost its original proof: %v", err)
					}
				}
				if cleanups.Load() != 1 {
					t.Fatalf("claim observations repeated original cleanup: %d", cleanups.Load())
				}
			})
		})
	}
}

func TestStartClaimsShareOriginalInstanceWorkCapacity(t *testing.T) {
	host := testHost(t, framehost.Config{})
	var lifetime frame.Lifetime
	var preparations, cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			lifetime = setup.Lifetime()
			preparations.Add(1)
			_, err := setup.Cleanup("original execution", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(t.Context(), "original-capacity")
	if err != nil {
		t.Fatal(err)
	}
	root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	first, err := host.StartUnit(t.Context(), grant, root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Stop(t.Context())
	var holds []frame.Work
	defer func() {
		for _, hold := range holds {
			hold.Finish()
		}
	}()
	for {
		hold, err := lifetime.Hold()
		if err != nil {
			if !errors.Is(err, frame.ErrCapacity) {
				t.Fatal(err)
			}
			break
		}
		holds = append(holds, hold)
	}
	if extra, err := host.StartUnit(t.Context(), grant, root); extra.Valid() || !errors.Is(err, frame.ErrCapacity) {
		if extra.Valid() {
			_ = extra.Stop(t.Context())
		}
		t.Errorf("start claim escaped its original instance capacity: valid=%t error=%v", extra.Valid(), err)
	}
	for _, hold := range holds {
		hold.Finish()
	}
	second, err := host.StartUnit(t.Context(), grant, root)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID() != first.ID() || preparations.Load() != 1 {
		t.Fatal("capacity refusal replaced the original preparation")
	}
	if err := first.Stop(t.Context()); err != nil || cleanups.Load() != 0 {
		t.Fatalf("one claim released another's original execution: cleanups=%d error=%v", cleanups.Load(), err)
	}
	if err := second.Stop(t.Context()); err != nil || cleanups.Load() != 1 {
		t.Fatalf("final claim lost original exit: cleanups=%d error=%v", cleanups.Load(), err)
	}
}

func TestPendingStartClaimsRetainTheirOriginalPreparation(t *testing.T) {
	for _, cancelAll := range []bool{false, true} {
		name := "one waiter remains"
		if cancelAll {
			name = "all waiters cancel"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				host := testHost(t, framehost.Config{})
				entered, release := make(chan struct{}), make(chan struct{})
				var preparations, cleanups atomic.Int32
				plugin := installNative(t, host, "worker", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
					Setup: func(ctx context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
						preparations.Add(1)
						if _, err := setup.Cleanup("original worker", func() error { cleanups.Add(1); return nil }); err != nil {
							return nil, err
						}
						close(entered)
						select {
						case <-release:
							return nil, nil
						case <-ctx.Done():
							return nil, frame.ContextError(ctx)
						}
					},
				})
				grant, err := host.Admission().OpenScope(t.Context(), "worker-owner")
				if err != nil {
					t.Fatal(err)
				}
				root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
				firstContext, cancelFirst := context.WithCancel(t.Context())
				defer cancelFirst()
				first := make(chan error, 1)
				go func() { _, err := host.StartUnit(firstContext, grant, root); first <- err }()
				<-entered
				secondContext, cancelSecond := context.WithCancel(t.Context())
				defer cancelSecond()
				type startResult struct {
					running framehost.RunningUnit
					err     error
				}
				second := make(chan startResult, 1)
				go func() {
					running, err := host.StartUnit(secondContext, grant, root)
					second <- startResult{running, err}
				}()
				synctest.Wait()
				cancelFirst()
				if err := <-first; !errors.Is(err, context.Canceled) {
					t.Fatalf("first wait did not cancel: %v", err)
				}
				synctest.Wait()
				if cleanups.Load() != 0 || preparations.Load() != 1 {
					t.Fatalf("one canceled claim changed another's preparation: preparations=%d, cleanups=%d", preparations.Load(), cleanups.Load())
				}
				if cancelAll {
					cancelSecond()
					result := <-second
					if result.running.Valid() || !errors.Is(result.err, context.Canceled) {
						t.Fatalf("second wait did not cancel: %+v", result)
					}
					synctest.Wait()
				} else {
					close(release)
					result := <-second
					if result.err != nil || !result.running.Valid() {
						t.Fatalf("original remaining claim did not start: %+v", result)
					}
					if err := result.running.Stop(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if cleanups.Load() != 1 || preparations.Load() != 1 {
					t.Errorf("original preparation did not retire once: preparations=%d, cleanups=%d", preparations.Load(), cleanups.Load())
				}
			})
		})
	}
}

func TestCanceledStartRetainsOriginalUnconfirmedExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := testHost(t, framehost.Config{})
		request, cancel := context.WithCancel(t.Context())
		defer cancel()
		var preparations, cleanups atomic.Int32
		var original frame.ResourceHandle
		plugin := installNative(t, host, "worker", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				generation := preparations.Add(1)
				resource, err := setup.Own(frame.Resource{Name: "original execution", Close: func(context.Context) (frame.ReleaseState, error) {
					cleanups.Add(1)
					if generation == 1 {
						return frame.ResourceUnconfirmed, nil
					}
					return frame.ResourceReleased, nil
				}})
				if err != nil {
					return nil, err
				}
				if generation == 1 {
					original = resource
				}
				return nil, setup.OnActivate(func(context.Context) error { cancel(); return nil })
			},
		})
		grant, err := host.Admission().OpenScope(t.Context(), "worker-owner")
		if err != nil {
			t.Fatal(err)
		}
		root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
		if running, err := host.StartUnit(request, grant, root); running.Valid() || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled start handed off its claim: valid=%v, error=%v", running.Valid(), err)
		}
		synctest.Wait()
		if cleanups.Load() != 1 {
			t.Fatalf("original preparation did not request exit: %d", cleanups.Load())
		}
		if running, err := host.StartUnit(t.Context(), grant, root); running.Valid() || !errors.Is(err, framehost.ErrInUse) {
			t.Fatalf("cancellation replaced an unconfirmed execution: valid=%v, error=%v", running.Valid(), err)
		}
		if err := original.ConfirmReleased(nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		next, err := host.StartUnit(t.Context(), grant, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := next.Stop(t.Context()); err != nil || cleanups.Load() != 2 || preparations.Load() != 2 {
			t.Errorf("late proof replaced or repeated original cleanup: preparations=%d, cleanups=%d, error=%v", preparations.Load(), cleanups.Load(), err)
		}
	})
}

func TestCanceledStartRetiresOnlyItsOriginalPreparation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := testHost(t, framehost.Config{})
		request, cancel := context.WithCancel(t.Context())
		defer cancel()
		var preparations, cleanups atomic.Int32
		plugin := installNative(t, host, "worker", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				preparations.Add(1)
				if _, err := setup.Cleanup("original worker", func() error { cleanups.Add(1); return nil }); err != nil {
					return nil, err
				}
				return nil, setup.OnActivate(func(context.Context) error { cancel(); return nil })
			},
		})
		grant, err := host.Admission().OpenScope(t.Context(), "worker-owner")
		if err != nil {
			t.Fatal(err)
		}
		root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
		if running, err := host.StartUnit(request, grant, root); running.Valid() || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request acquired a start claim: valid=%v, error=%v", running.Valid(), err)
		}
		synctest.Wait()
		if cleanups.Load() != 1 {
			t.Errorf("canceled start left its original preparation running: cleanups=%d, instances=%+v", cleanups.Load(), host.Observations())
		}
		if err := host.Admission().ValidateScope(grant); err != nil {
			t.Fatalf("canceled request fenced another owner's permission: %v", err)
		}
		next, err := host.StartUnit(t.Context(), grant, root)
		if err != nil {
			t.Fatal(err)
		}
		if preparations.Load() != 2 {
			t.Errorf("fresh start adopted abandoned preparation: preparations=%d", preparations.Load())
		}
		if err := next.Stop(t.Context()); err != nil || cleanups.Load() != 2 {
			t.Errorf("fresh start did not own its original retirement: cleanups=%d, error=%v", cleanups.Load(), err)
		}
	})
}

func TestRunningUnitCopiesShareOneStartClaim(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(ctx, "worker-owner")
	if err != nil {
		t.Fatal(err)
	}
	root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	first, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	copied := first
	stopped := make(chan error, 2)
	go func() { stopped <- first.Stop(ctx) }()
	go func() { stopped <- copied.Stop(ctx) }()
	for range cap(stopped) {
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
	}
	if first.ID() != copied.ID() || first.ID() != second.ID() || cleanups.Load() != 0 {
		t.Fatalf("copies consumed an independent start claim: first=%s, copy=%s, second=%s, cleanups=%d", first.ID(), copied.ID(), second.ID(), cleanups.Load())
	}
	third, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatalf("copied stop withdrew the shared instance: %v", err)
	}
	if third.ID() != second.ID() {
		t.Fatal("copy stopping replaced the original instance")
	}
	if err := second.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if cleanups.Load() != 0 {
		t.Fatal("an independent claim stopped the remaining client")
	}
	if err := third.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stoppedCopy := copied
	if err := stoppedCopy.Stop(ctx); err != nil || cleanups.Load() != 1 {
		t.Fatalf("original retirement changed: cleanups=%d, error=%v", cleanups.Load(), err)
	}
}

func TestOriginalStartClaimsCannotWithdrawSuccessor(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(ctx, "worker-owner")
	if err != nil {
		t.Fatal(err)
	}
	root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	original, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.RetireInstance(ctx, original.ID()); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitInstanceRetired(ctx, original.ID()); err != nil {
		t.Fatal(err)
	}
	successor, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	if successor.ID() == original.ID() || other.ID() != original.ID() || cleanups.Load() != 1 {
		t.Fatal("successor did not follow the original instance's confirmed exit")
	}
	for _, claim := range []framehost.RunningUnit{original, other} {
		if err := claim.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	current, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID() != successor.ID() || cleanups.Load() != 1 {
		t.Fatal("an original start claim withdrew its successor")
	}
	for _, claim := range []framehost.RunningUnit{current, successor} {
		if err := claim.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if cleanups.Load() != 2 {
		t.Fatalf("original and successor cleanup count = %d", cleanups.Load())
	}
}
