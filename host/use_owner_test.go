package host_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

type retirementReentryError struct {
	use     *framehost.Use
	once    sync.Once
	reenter func()
}

func (e *retirementReentryError) Error() string {
	if outcome, closing := e.use.Lifetime().CloseOutcome(); closing && outcome.Released {
		e.once.Do(e.reenter)
	}
	return "original cleanup diagnostic"
}

func TestReleasedUseDropsOriginalEdgesBeforeForwardingDiagnostics(t *testing.T) {
	ctx := testContext(t)
	host, err := framehost.New(ctx, framehost.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var reverse *framehost.Use
	t.Cleanup(func() {
		if reverse != nil {
			if outcome, err := reverse.Close(ctx); !outcome.Released || err != nil {
				t.Errorf("reverse use close: %+v, %v", outcome, err)
			}
		}
		if outcome, err := host.Close(ctx); !outcome.Released || err != nil && !errors.Is(err, frame.ErrCloseFailed) {
			t.Errorf("host close: %+v, %v", outcome, err)
		}
	})
	var left, right frame.Lifetime
	plugin := installNative(t, host, "owners", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "left", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
			left = setup.Lifetime()
			return nil, nil
		},
	}, framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "right", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
			right = setup.Lifetime()
			return nil, nil
		},
	})
	leftRoot := framehost.UnitRef{InstallationID: "owners", Release: plugin.Revision(), UnitID: "left"}
	rightRoot := framehost.UnitRef{InstallationID: "owners", Release: plugin.Revision(), UnitID: "right"}
	for _, root := range []framehost.UnitRef{leftRoot, rightRoot} {
		if _, err := host.ActivateUnit(ctx, "", root); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := host.Admission().OpenScope(ctx, "borrow")
	if err != nil {
		t.Fatal(err)
	}
	use, err := host.AcquireUnit(ctx, rightRoot, framehost.UseConfig{Owner: left, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	var acquireErr error
	cause := &retirementReentryError{use: use, reenter: func() {
		reverse, acquireErr = host.AcquireUnit(ctx, leftRoot, framehost.UseConfig{Owner: right, Scope: scope})
	}}
	if _, err := use.Lifetime().Own(frame.Resource{Name: "diagnostic", Close: func(context.Context) (frame.ReleaseState, error) {
		return frame.ResourceReleased, cause
	}}); err != nil {
		t.Fatal(err)
	}
	if outcome, err := use.Close(ctx); !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("original Use close: %+v, %v", outcome, err)
	}
	if reverse == nil || acquireErr != nil {
		t.Fatalf("released edge still governed diagnostic reentry: %v", acquireErr)
	}
}

func TestDynamicUseCyclesAreRejectedWithoutRetainingInstances(t *testing.T) {
	for _, size := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d-owners", size), func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, framehost.Config{})
			owners := make([]frame.Lifetime, size)
			roots := make([]framehost.UnitRef, size)
			for index := range size {
				id := framehost.InstallationID(fmt.Sprintf("owner-%d", index))
				plugin := installNative(t, host, string(id), framehost.Unit{
					Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
					Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
						owners[index] = setup.Lifetime()
						return nil, nil
					},
				})
				roots[index] = framehost.UnitRef{InstallationID: id, Release: plugin.Revision(), UnitID: "main"}
				if _, err := host.ActivateUnit(ctx, "", roots[index]); err != nil {
					t.Fatal(err)
				}
			}
			_, scope := ownerAndScope(t, host)
			for index := range size - 1 {
				if _, err := host.AcquireUnit(ctx, roots[index+1], framehost.UseConfig{Owner: owners[index], Scope: scope}); err != nil {
					t.Fatalf("acyclic ownership refused: %v", err)
				}
			}
			use, err := host.AcquireUnit(ctx, roots[0], framehost.UseConfig{Owner: owners[size-1], Scope: scope})
			if use != nil {
				_, _ = use.Close(ctx)
			}
			if !errors.Is(err, framehost.ErrDependencyCycle) || use != nil {
				t.Fatalf("dynamic cycle admitted: use=%v error=%v", use, err)
			}
			for _, root := range roots {
				if err := host.Retire(ctx, root.InstallationID); err != nil {
					t.Fatal(err)
				}
			}
			for _, root := range roots {
				if err := host.WaitRetired(ctx, root.InstallationID); err != nil {
					t.Fatalf("rejected cycle retained %s: %v", root.InstallationID, err)
				}
			}
		})
	}
}

func TestUseAdmissionRejectsOriginalOwnerCycles(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	service, err := framehost.NewContract[string]("owner.provider", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	setups := make(map[framehost.UnitID]*framehost.Setup)
	prepare := func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
		setups[setup.Unit().ID] = setup
		if setup.Unit().ID == "provider" {
			return []framehost.ExportValue{framehost.Provide("service", service, "value")}, nil
		}
		return nil, nil
	}
	provider := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "provider", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "service", Contract: service.Ref()}}}, Setup: prepare,
	})
	providerSource := source("provider", provider, "provider", "service", service.Ref())
	consumer := installNative(t, host, "consumer", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "consumer", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "service", Contract: service.Ref(), Providers: []framehost.ExportRef{providerSource}}}}, Setup: prepare,
	})
	independent := installNative(t, host, "independent", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "independent", Profile: framehost.NativeGo}, Setup: prepare,
	})
	refs := map[framehost.UnitID]framehost.UnitRef{
		"provider":    providerSource.Unit(),
		"consumer":    {InstallationID: "consumer", Release: consumer.Revision(), UnitID: "consumer"},
		"independent": {InstallationID: "independent", Release: independent.Revision(), UnitID: "independent"},
	}
	for _, id := range []framehost.UnitID{"consumer", "independent"} {
		if _, err := host.ActivateUnit(ctx, "", refs[id]); err != nil {
			t.Fatal(err)
		}
	}
	owner, scope := ownerAndScope(t, host)
	acquire := func(id framehost.UnitID, config framehost.UseConfig) (*framehost.Use, error) {
		t.Helper()
		plan, err := host.PlanUnit(ctx, "", refs[id], nil)
		if err != nil {
			t.Fatal(err)
		}
		return host.Acquire(ctx, plan, config)
	}
	for _, id := range []framehost.UnitID{"provider", "consumer"} {
		if use, err := acquire(id, framehost.UseConfig{Owner: setups["provider"].Lifetime(), Scope: scope}); !errors.Is(err, framehost.ErrDependencyCycle) || use != nil {
			t.Fatalf("owner cycle accepted for %s: use=%v error=%v", id, use, err)
		}
	}
	parent, err := acquire("independent", framehost.UseConfig{Owner: setups["provider"].Lifetime(), Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if use, err := acquire("consumer", framehost.UseConfig{Owner: parent.Lifetime(), Parent: parent}); !errors.Is(err, framehost.ErrDependencyCycle) || use != nil {
		t.Fatalf("nested owner cycle accepted: use=%v error=%v", use, err)
	}
	if _, err := parent.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, actualOwner := range []frame.Lifetime{owner, setups["consumer"].Lifetime()} {
		use, err := acquire("provider", framehost.UseConfig{Owner: actualOwner, Scope: scope})
		if err != nil {
			t.Fatalf("acyclic consumer ownership rejected: %v", err)
		}
		if _, err := use.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []framehost.UnitID{"consumer", "provider", "independent"} {
		if err := host.RetireInstance(ctx, setups[id].InstanceID()); err != nil {
			t.Fatal(err)
		}
		if err := host.WaitInstanceRetired(ctx, setups[id].InstanceID()); err != nil {
			t.Fatalf("rejected Use retained %s: %v", id, err)
		}
	}
}
