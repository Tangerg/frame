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

type recordedUseRelease struct {
	id    string
	proof frame.ReleaseProof
}

type useRetirementProtection struct {
	approve      func() error
	approval     framehost.UseApproval
	releases     chan recordedUseRelease
	releaseCalls atomic.Int64
}

func (p *useRetirementProtection) ApproveUse(_ context.Context, approval framehost.UseApproval) error {
	p.approval = approval
	return p.approve()
}

func (p *useRetirementProtection) SettleUse(_ context.Context, id string, proof frame.ReleaseProof) error {
	p.releaseCalls.Add(1)
	p.releases <- recordedUseRelease{id: id, proof: proof}
	return nil
}

func TestUseRetiresWhenItsOriginalLifetimeCloses(t *testing.T) {
	for _, test := range []struct {
		name  string
		close func(context.Context, *framehost.Use) (frame.CloseOutcome, error)
	}{
		{"use close", func(ctx context.Context, use *framehost.Use) (frame.CloseOutcome, error) {
			return use.Close(ctx)
		}},
		{"lifetime close", func(ctx context.Context, use *framehost.Use) (frame.CloseOutcome, error) {
			return use.Lifetime().Close(ctx)
		}},
		{"lifetime begin close", func(ctx context.Context, use *framehost.Use) (frame.CloseOutcome, error) {
			return use.Lifetime().BeginClose(frame.ErrClosed).Wait(ctx)
		}},
	} {
		for _, lateProof := range []bool{false, true} {
			name := "cleanup proof"
			if lateProof {
				name = "late proof"
			}
			t.Run(test.name+"/"+name, func(t *testing.T) {
				ctx := testContext(t)
				host := testHost(t, framehost.Config{Limits: framehost.Limits{MaxUses: 1}})
				plugin := installNative(t, host, "provider", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
					Setup:       func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil },
				})
				plan, err := host.PlanUnit(ctx, "", framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				owner, scope := ownerAndScope(t, host)
				config := framehost.UseConfig{Owner: owner, Scope: scope}
				use, err := host.Acquire(ctx, plan, config)
				if err != nil {
					t.Fatal(err)
				}
				var closes atomic.Int64
				resource, err := use.Lifetime().Own(frame.Resource{Name: "original resource", Close: func(context.Context) (frame.ReleaseState, error) {
					closes.Add(1)
					if lateProof {
						return frame.ResourceUnconfirmed, nil
					}
					return frame.ResourceReleased, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				first, err := test.close(ctx, use)
				if lateProof {
					if first.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) {
						t.Fatalf("unconfirmed resource released its Use: %+v, %v", first, err)
					}
					if _, err := host.Acquire(ctx, plan, config); !errors.Is(err, frame.ErrCapacity) {
						t.Fatalf("unconfirmed Use released capacity: %v", err)
					}
					if err := resource.ConfirmReleased(nil); err != nil {
						t.Fatal(err)
					}
				} else if !first.Released || err != nil {
					t.Fatalf("resource close: %+v, %v", first, err)
				}
				retired, err := use.WaitRetired(ctx)
				if err != nil || !retired.Released || retired.ID != first.ID || closes.Load() != 1 {
					t.Fatalf("original close did not retire its Use once: %+v, %v, cleanups=%d", retired, err, closes.Load())
				}
				next, err := host.Acquire(ctx, plan, config)
				if err != nil {
					t.Fatalf("retired Use retained Host capacity: %v", err)
				}
				if _, err := next.Close(ctx); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestConfirmedUseRetirementWinsCanceledWait(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	plugin := installNative(t, host, "retired", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup:       func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil },
	})
	owner, scope := ownerAndScope(t, host)
	use, err := host.AcquireUnit(ctx, framehost.UnitRef{InstallationID: "retired", Release: plugin.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	original, err := use.Close(ctx)
	if err != nil || !original.Released {
		t.Fatalf("original Use did not retire: %+v, %v", original, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for range 128 {
		outcome, err := use.WaitRetired(canceled)
		if err != nil || !outcome.Released || outcome.ID != original.ID {
			t.Fatalf("cancelled observation replaced original completion: %+v, %v", outcome, err)
		}
	}
}

func TestUseOwnerCancellationSettlesOnceAcrossAdmission(t *testing.T) {
	for _, duringAdmission := range []bool{false, true} {
		name := "published use"
		if duringAdmission {
			name = "refused publication"
		}
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			owner := frame.NewLifetime(parent)
			protection := &useRetirementProtection{releases: make(chan recordedUseRelease, 1)}
			protection.approve = func() error {
				if !duringAdmission {
					return nil
				}
				cancel()
				select {
				case <-owner.Context().Done():
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			host := testHost(t, framehost.Config{ExecutionProtection: protection})
			t.Cleanup(func() {
				if outcome, err := owner.Close(testContext(t)); err != nil || !outcome.Released {
					t.Errorf("owner close: %+v, %v", outcome, err)
				}
			})
			var providerCloses atomic.Int64
			plugin := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
					_, err := setup.Own(frame.Resource{Name: "provider resource", Close: func(context.Context) (frame.ReleaseState, error) {
						providerCloses.Add(1)
						return frame.ResourceReleased, nil
					}})
					return nil, err
				},
			})
			scope, err := host.Admission().OpenScope(ctx, "test-caller")
			if err != nil {
				t.Fatal(err)
			}
			unit := framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
			use, err := host.AcquireUnit(ctx, unit, framehost.UseConfig{Owner: owner, Scope: scope, Realm: "test-realm", ExecutionID: "test-execution"})
			var outcome frame.CloseOutcome
			if duringAdmission {
				if use != nil || !errors.Is(err, frame.ErrClosed) {
					t.Fatalf("closed owner accepted publication: %v, %v", use, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				select {
				case <-use.Context().Done():
				case <-ctx.Done():
					t.Fatal("owner cancellation did not close its Use")
				}
				if use.BeginClose(frame.ErrClosed) != use.Lifetime().BeginClose(frame.ErrClosed) {
					t.Fatal("close entry points created another operation")
				}
				outcome, err = use.WaitRetired(ctx)
				if err != nil || !outcome.Released {
					t.Fatalf("cancelled owner retained its Use: %+v, %v", outcome, err)
				}
			}
			select {
			case release := <-protection.releases:
				if release.id != protection.approval.ID || release.proof.CloseID == 0 || !duringAdmission && release.proof.CloseID != outcome.ID {
					t.Fatalf("settled another approval or close: %+v", release)
				}
			case <-ctx.Done():
				t.Fatal("original approved Use was never released")
			}
			if outcome, err := host.Close(ctx); err != nil || !outcome.Released {
				t.Fatalf("retirement retained provider holds: %+v, %v", outcome, err)
			}
			if protection.releaseCalls.Load() != 1 || providerCloses.Load() != 1 {
				t.Fatalf("retirement repeated: settlements=%d, provider cleanups=%d", protection.releaseCalls.Load(), providerCloses.Load())
			}
		})
	}
}

func TestUseFollowsInstallationScopeForPreactivatedInstance(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		captured bool
		draining bool
	}{
		{name: "declaration plan"},
		{name: "captured plan", captured: true},
		{name: "draining declaration plan", draining: true},
		{name: "draining captured plan", captured: true, draining: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := testContext(t)
				host := testHost(t, framehost.Config{})
				point, err := framehost.NewPoint[string, string]("scope.original", "1")
				if err != nil {
					t.Fatal(err)
				}
				if err := host.Register(point); err != nil {
					t.Fatal(err)
				}
				var grant framehost.ScopeGrant
				var instanceID string
				var cleanups atomic.Int32
				plugin := installNative(t, host, "provider", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: point.Ref()}}},
					Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
						grant, instanceID = setup.ScopeGrant(), setup.InstanceID()
						_, err := setup.Cleanup("original provider", func() error { cleanups.Add(1); return nil })
						return []framehost.ExportValue{framehost.Contribute("value", point, "metadata", "original")}, err
					},
				})
				root := source("provider", plugin, "main", "value", point.Ref())
				if _, err := host.Activate(ctx, root); err != nil {
					t.Fatal(err)
				}
				plan, err := host.Plan(ctx, root)
				if scenario.captured {
					snapshot, snapshotErr := host.Snapshot(ctx, point.Ref())
					if snapshotErr != nil {
						t.Fatal(snapshotErr)
					}
					candidates, selectionErr := framehost.Candidates(snapshot, point)
					if selectionErr != nil {
						t.Fatal(selectionErr)
					}
					selection, selectionErr := framehost.Select(snapshot, point, "original", candidates...)
					if selectionErr != nil {
						t.Fatal(selectionErr)
					}
					plan, err = framehost.Capture(snapshot, selection.Ref())
				}
				if err != nil {
					t.Fatal(err)
				}
				owner := frame.NewLifetime(t.Context())
				defer owner.Close(context.Background())
				use, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: grant})
				if err != nil {
					t.Fatal(err)
				}
				defer use.Close(context.Background())
				originalContext := use.Context()
				if scenario.draining {
					if err := host.RetireInstance(ctx, instanceID); err != nil {
						t.Fatal(err)
					}
				}
				if err := host.SetEnabled(ctx, "provider", false); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				select {
				case <-originalContext.Done():
				default:
					t.Error("revoked original installation scope retained its Use")
					return
				}
				if !errors.Is(context.Cause(originalContext), framehost.ErrFenced) {
					t.Fatal("Use lost its original scope fence")
				}
				if outcome, err := use.WaitRetired(ctx); !outcome.Released || err != nil {
					t.Fatalf("revoked Use did not retire: %+v, %v", outcome, err)
				}
				if err := host.WaitInstanceRetired(ctx, instanceID); err != nil || cleanups.Load() != 1 {
					t.Fatalf("original instance retained or repeated cleanup: cleanups=%d error=%v", cleanups.Load(), err)
				}
			})
		})
	}
}

func TestInstallationScopeFenceKeepsOriginalFailedSettlement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := testContext(t)
		failure := errors.New("original settlement acknowledgement lost")
		protection := &reservedUseProtection{failures: 1, failure: failure, releases: make(chan recordedUseRelease, 2)}
		host := testHost(t, framehost.Config{ExecutionProtection: protection})
		var grant framehost.ScopeGrant
		var instanceID string
		var cleanups atomic.Int32
		plugin := installNative(t, host, "provider", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
			Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
				grant, instanceID = setup.ScopeGrant(), setup.InstanceID()
				_, err := setup.Cleanup("original provider", func() error { cleanups.Add(1); return nil })
				return nil, err
			},
		})
		root := framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
		if _, err := host.ActivateUnit(ctx, "", root); err != nil {
			t.Fatal(err)
		}
		plan, err := host.PlanUnit(ctx, "realm", root, nil)
		if err != nil {
			t.Fatal(err)
		}
		owner := frame.NewLifetime(t.Context())
		defer owner.Close(context.Background())
		use, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: grant, Realm: "realm", ExecutionID: "original-execution"})
		if err != nil {
			t.Fatal(err)
		}
		defer use.Close(context.Background())
		originalContext := use.Context()
		if err := host.SetEnabled(ctx, "provider", false); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-originalContext.Done():
		default:
			t.Error("original scope fence did not start Use settlement")
			return
		}
		if protection.releaseCalls.Load() != 1 || cleanups.Load() != 0 || !errors.Is(host.Admission().RecoveryError(), failure) {
			t.Fatalf("failed settlement retried or released its provider: calls=%d cleanups=%d", protection.releaseCalls.Load(), cleanups.Load())
		}
		original, err := use.BeginClose(frame.ErrClosed).WaitReleased(ctx)
		if !original.Released || err != nil {
			t.Fatalf("scope exit lost its physical proof: %+v, %v", original, err)
		}
		if outcome, err := host.Close(ctx); !outcome.Released || err != nil {
			t.Fatalf("explicit Host close did not settle the original Use: %+v, %v", outcome, err)
		}
		if outcome, err := use.WaitRetired(ctx); !outcome.Released || outcome.ID != original.ID || err != nil {
			t.Fatalf("settlement replaced the original exit: %+v, %v", outcome, err)
		}
		if err := host.WaitInstanceRetired(ctx, instanceID); err != nil || cleanups.Load() != 1 || protection.releaseCalls.Load() != 2 {
			t.Fatalf("original cleanup or retry count changed: cleanups=%d calls=%d error=%v", cleanups.Load(), protection.releaseCalls.Load(), err)
		}
		for range 2 {
			release := <-protection.releases
			if release.id != use.ID() || release.proof.CloseID != original.ID {
				t.Fatalf("retry settled another approval or proof: %+v", release)
			}
		}
	})
}

func TestUseRetiresAfterOriginalAuthorityCancellation(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		authority   string
		settleFails bool
	}{
		{name: "host", authority: "host"},
		{name: "owner", authority: "owner"},
		{name: "instance", authority: "instance"},
		{name: "provider", authority: "provider"},
		{name: "host with settlement failure", authority: "host", settleFails: true},
		{name: "owner with settlement failure", authority: "owner", settleFails: true},
		{name: "instance with settlement failure", authority: "instance", settleFails: true},
		{name: "provider with settlement failure", authority: "provider", settleFails: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := testContext(t)
				protection := &reservedUseProtection{failure: errors.New("original settlement failed"), releases: make(chan recordedUseRelease, 2)}
				if scenario.settleFails {
					protection.failures = 1
				}
				hostLifetime := frame.NewLifetime(t.Context())
				host := testHost(t, framehost.Config{Lifetime: hostLifetime, ExecutionProtection: protection})
				input, err := framehost.NewContract[string]("execution.original", "1")
				if err != nil {
					t.Fatal(err)
				}
				if err := host.Register(input); err != nil {
					t.Fatal(err)
				}
				var providerLifetime, instanceLifetime frame.Lifetime
				var cleanups atomic.Int32
				provider := installNative(t, host, "provider", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: input.Ref()}}},
					Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
						providerLifetime = setup.Lifetime()
						_, err := setup.Cleanup("original provider", func() error { cleanups.Add(1); return nil })
						return []framehost.ExportValue{framehost.Provide("value", input, "original")}, err
					},
				})
				consumer := installNative(t, host, "consumer", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "input", Contract: input.Ref(), Providers: []framehost.ExportRef{source("provider", provider, "main", "value", input.Ref())}}}},
					Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
						instanceLifetime = setup.Lifetime()
						_, err := setup.Cleanup("original consumer", func() error { cleanups.Add(1); return nil })
						return nil, err
					},
				})
				owner, grant := ownerAndScope(t, host)
				root := framehost.UnitRef{InstallationID: "consumer", Release: consumer.Revision(), UnitID: "main"}
				use, err := host.AcquireUnit(ctx, root, framehost.UseConfig{Owner: owner, Scope: grant, Realm: "realm", ExecutionID: "original-execution"})
				if err != nil {
					t.Fatal(err)
				}
				defer use.Close(context.Background())
				originalContext := use.Context()
				stopped := instanceLifetime
				switch scenario.authority {
				case "host":
					stopped = hostLifetime
				case "owner":
					stopped = owner
				case "provider":
					stopped = providerLifetime
				}
				cause := errors.New("original execution stopped")
				stopped.Cancel(cause)
				synctest.Wait()
				select {
				case <-originalContext.Done():
				default:
					t.Error("ended execution kept its original Use open")
					return
				}
				if !errors.Is(context.Cause(originalContext), cause) || protection.releaseCalls.Load() != 1 {
					t.Fatalf("execution cancellation lost original cause or settlement: calls=%d", protection.releaseCalls.Load())
				}
				if scenario.settleFails && (cleanups.Load() != 0 || !errors.Is(host.Admission().RecoveryError(), protection.failure)) {
					t.Fatal("failed original settlement released its held execution")
				}
				original, err := use.BeginClose(frame.ErrClosed).WaitReleased(ctx)
				if !original.Released || err != nil {
					t.Fatalf("original Use resource exit: %+v, %v", original, err)
				}
				if outcome, err := host.Close(ctx); !outcome.Released || err != nil {
					t.Fatalf("original execution retirement: %+v, %v", outcome, err)
				}
				expectedReleases := int64(1)
				if scenario.settleFails {
					expectedReleases++
				}
				if cleanups.Load() != 2 || protection.releaseCalls.Load() != expectedReleases {
					t.Fatalf("original cleanup or settlement repeated: cleanups=%d settlements=%d", cleanups.Load(), protection.releaseCalls.Load())
				}
				for range expectedReleases {
					release := <-protection.releases
					if release.id != use.ID() || release.proof.CloseID != original.ID {
						t.Fatalf("settlement replaced its original approval or exit: %+v", release)
					}
				}
			})
		})
	}
}
