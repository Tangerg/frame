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

type reservedUseProtection struct {
	approve      func() error
	approval     framehost.UseApproval
	approvals    atomic.Int64
	releaseCalls atomic.Int64
	failures     int64
	failure      error
	releases     chan recordedUseRelease
}

func (p *reservedUseProtection) ApproveUse(_ context.Context, approval framehost.UseApproval) error {
	p.approvals.Add(1)
	p.approval = approval
	if p.approve != nil {
		return p.approve()
	}
	return nil
}

func (p *reservedUseProtection) SettleUse(_ context.Context, id string, proof frame.ReleaseProof) error {
	calls := p.releaseCalls.Add(1)
	p.releases <- recordedUseRelease{id: id, proof: proof}
	if calls <= p.failures {
		return p.failure
	}
	return nil
}

func TestUnpublishedUseKeepsItsOriginalSettlement(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		hostOwned bool
		closeHost bool
	}{
		{name: "owner closes during approval"},
		{name: "host closes during approval", closeHost: true},
		{name: "host-owned use closes during approval", hostOwned: true, closeHost: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lifetime, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxResources: 2})
				if err != nil {
					t.Fatal(err)
				}
				owner := lifetime
				if !scenario.hostOwned {
					owner = frame.NewLifetime(t.Context())
				}
				failure := errors.New("temporary use release failure")
				protection := &reservedUseProtection{failures: 2, failure: failure, releases: make(chan recordedUseRelease, 3)}
				protection.approve = func() error {
					if scenario.closeHost {
						lifetime.BeginClose(frame.ErrClosed)
					} else {
						owner.BeginClose(frame.ErrClosed)
					}
					return nil
				}
				host, err := framehost.New(t.Context(), framehost.Config{Lifetime: lifetime, ExecutionProtection: protection, Limits: framehost.Limits{MaxUses: 1}})
				if err != nil {
					t.Fatal(err)
				}
				var cleanups atomic.Int64
				plugin := installNative(t, host, "provider", framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
					Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
						_, err := setup.Cleanup("original provider", func() error { cleanups.Add(1); return nil })
						return nil, err
					},
				})
				scope, err := host.Admission().OpenScope(t.Context(), "consumer")
				if err != nil {
					t.Fatal(err)
				}
				root := framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
				use, err := host.AcquireUnit(t.Context(), root, framehost.UseConfig{Owner: owner, Scope: scope, Realm: "realm"})
				if use != nil || !errors.Is(err, frame.ErrClosed) {
					t.Errorf("closed owner published its grant: %v, %v", use, err)
				}
				synctest.Wait()
				if calls := protection.releaseCalls.Load(); calls != 1 || cleanups.Load() != 0 {
					t.Errorf("unpublished settlement was retried or released early: calls=%d, cleanups=%d", calls, cleanups.Load())
				}
				if err := host.Admission().RecoveryError(); !errors.Is(err, framehost.ErrRecoveryRequired) {
					t.Errorf("failed release left admission open: %v", err)
				}
				if err := host.Admission().Recover(t.Context(), func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				outcome, err := host.Close(testContext(t))
				synctest.Wait()
				if outcome.Released || !errors.Is(err, failure) || protection.releaseCalls.Load() != 2 || cleanups.Load() != 0 {
					t.Errorf("one close did not preserve exactly its failed attempt: %+v, %v, calls=%d, cleanups=%d", outcome, err, protection.releaseCalls.Load(), cleanups.Load())
				}
				if err := host.Admission().Recover(t.Context(), func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				outcome, err = host.Close(testContext(t))
				if !outcome.Released || err != nil && !errors.Is(err, frame.ErrCloseFailed) {
					t.Errorf("host could not retire its original unpublished use: %+v, %v", outcome, err)
				}
				synctest.Wait()
				if protection.approvals.Load() != 1 || protection.releaseCalls.Load() != 3 || cleanups.Load() != 1 {
					t.Errorf("recovery replaced or repeated the original operation: approvals=%d, releases=%d, cleanups=%d", protection.approvals.Load(), protection.releaseCalls.Load(), cleanups.Load())
				}
				var original frame.ReleaseProof
				for range 3 {
					release := <-protection.releases
					if original.CloseID == 0 {
						original = release.proof
					}
					if release.id != protection.approval.ID || release.proof != original || release.proof.CloseID == 0 {
						t.Errorf("recovery changed the original approval or proof: %+v", release)
					}
				}
				if outcome, err := owner.Close(testContext(t)); !outcome.Released || err != nil && !errors.Is(err, frame.ErrCloseFailed) {
					t.Errorf("original owner stayed pending: %+v, %v", outcome, err)
				}
			})
		})
	}
}

func TestUseReservesLocalCapacityBeforeProtection(t *testing.T) {
	for _, hostFull := range []bool{false, true} {
		name := "owner capacity"
		if hostFull {
			name = "host capacity"
		}
		t.Run(name, func(t *testing.T) {
			hostLimit := 2
			if hostFull {
				hostLimit = 1
			}
			lifetime, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxResources: hostLimit})
			if err != nil {
				t.Fatal(err)
			}
			protection := &reservedUseProtection{releases: make(chan recordedUseRelease, 1)}
			host := testHost(t, framehost.Config{Lifetime: lifetime, ExecutionProtection: protection})
			owner, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxResources: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !hostFull {
				if _, err := owner.Cleanup("occupied", func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			plugin := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
				Setup:       func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil },
			})
			scope, err := host.Admission().OpenScope(t.Context(), "consumer")
			if err != nil {
				t.Fatal(err)
			}
			root := framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
			use, err := host.AcquireUnit(t.Context(), root, framehost.UseConfig{Owner: owner, Scope: scope, Realm: "realm"})
			if use != nil || !errors.Is(err, frame.ErrCapacity) || protection.approvals.Load() != 0 {
				t.Fatalf("protection preceded its local capacity: %v, %v, approvals=%d", use, err, protection.approvals.Load())
			}
			if outcome, err := owner.Close(testContext(t)); err != nil || !outcome.Released {
				t.Fatalf("refused reservation leaked owner resources: %+v, %v", outcome, err)
			}
			if outcome, err := host.Close(testContext(t)); err != nil || !outcome.Released {
				t.Fatalf("refused reservation retained its provider: %+v, %v", outcome, err)
			}
			if protection.releaseCalls.Load() != 0 {
				t.Fatal("unapproved use attempted durable release")
			}
		})
	}
}
