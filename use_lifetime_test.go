package frame_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

type recordedUseRelease struct {
	id    string
	proof frame.ReleaseProof
}

type useRetirementProtection struct {
	approve      func() error
	approval     frame.UseApproval
	releases     chan recordedUseRelease
	releaseCalls atomic.Int64
}

func (p *useRetirementProtection) ApproveUse(_ context.Context, approval frame.UseApproval) error {
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
		close func(context.Context, *frame.Use) (frame.CloseOutcome, error)
	}{
		{"use close", func(ctx context.Context, use *frame.Use) (frame.CloseOutcome, error) {
			return use.Close(ctx)
		}},
		{"lifetime close", func(ctx context.Context, use *frame.Use) (frame.CloseOutcome, error) {
			return use.Lifetime().Close(ctx)
		}},
		{"lifetime begin close", func(ctx context.Context, use *frame.Use) (frame.CloseOutcome, error) {
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
				host := testHost(t, frame.HostConfig{Limits: frame.HostLimits{MaxUses: 1}})
				plugin := installNative(t, host, "provider", frame.Unit{
					Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
					Setup:       func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil },
				})
				plan, err := host.PlanUnit(ctx, "", frame.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				owner, scope := ownerAndScope(t, host)
				config := frame.UseConfig{Owner: owner, Scope: scope}
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
			host := testHost(t, frame.HostConfig{ExecutionProtection: protection})
			t.Cleanup(func() {
				if outcome, err := owner.Close(testContext(t)); err != nil || !outcome.Released {
					t.Errorf("owner close: %+v, %v", outcome, err)
				}
			})
			var providerCloses atomic.Int64
			plugin := installNative(t, host, "provider", frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
				Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
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
			unit := frame.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
			use, err := host.AcquireUnit(ctx, unit, frame.UseConfig{Owner: owner, Scope: scope, Realm: "test-realm", ExecutionID: "test-execution"})
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
