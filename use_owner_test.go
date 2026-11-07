package frame_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tangerg/frame"
)

func TestDynamicUseCyclesAreRejectedWithoutRetainingInstances(t *testing.T) {
	for _, size := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d-owners", size), func(t *testing.T) {
			ctx := testContext(t)
			host := testHost(t, frame.HostConfig{})
			owners := make([]*frame.Lifetime, size)
			roots := make([]frame.UnitRef, size)
			for index := range size {
				id := frame.InstallationID(fmt.Sprintf("owner-%d", index))
				plugin := installNative(t, host, string(id), frame.Unit{
					Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
					Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
						owners[index] = setup.Lifetime()
						return nil, nil
					},
				})
				roots[index] = frame.UnitRef{InstallationID: id, Release: plugin.Revision(), UnitID: "main"}
				if _, err := host.ActivateUnit(ctx, "", roots[index]); err != nil {
					t.Fatal(err)
				}
			}
			_, scope := ownerAndScope(t, host)
			for index := range size - 1 {
				if _, err := host.AcquireUnit(ctx, roots[index+1], frame.UseConfig{Owner: owners[index], Scope: scope}); err != nil {
					t.Fatalf("acyclic ownership refused: %v", err)
				}
			}
			use, err := host.AcquireUnit(ctx, roots[0], frame.UseConfig{Owner: owners[size-1], Scope: scope})
			if use != nil {
				_, _ = use.Close(ctx)
			}
			if !errors.Is(err, frame.ErrDependencyCycle) || use != nil {
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
	host := testHost(t, frame.HostConfig{})
	service, err := frame.NewContract[string]("owner.provider", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	setups := make(map[frame.UnitID]*frame.Setup)
	prepare := func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
		setups[setup.Unit().ID] = setup
		if setup.Unit().ID == "provider" {
			return []frame.ExportValue{frame.Provide("service", service, "value")}, nil
		}
		return nil, nil
	}
	provider := installNative(t, host, "provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "provider", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "service", Contract: service.Ref()}}}, Setup: prepare,
	})
	providerSource := source("provider", provider, "provider", "service", service.Ref())
	consumer := installNative(t, host, "consumer", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "consumer", Profile: frame.NativeGo, Imports: []frame.ImportDeclaration{{Slot: "service", Contract: service.Ref(), Providers: []frame.ExportRef{providerSource}}}}, Setup: prepare,
	})
	independent := installNative(t, host, "independent", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "independent", Profile: frame.NativeGo}, Setup: prepare,
	})
	refs := map[frame.UnitID]frame.UnitRef{
		"provider":    providerSource.Unit(),
		"consumer":    {InstallationID: "consumer", Release: consumer.Revision(), UnitID: "consumer"},
		"independent": {InstallationID: "independent", Release: independent.Revision(), UnitID: "independent"},
	}
	for _, id := range []frame.UnitID{"consumer", "independent"} {
		if _, err := host.ActivateUnit(ctx, "", refs[id]); err != nil {
			t.Fatal(err)
		}
	}
	owner, scope := ownerAndScope(t, host)
	acquire := func(id frame.UnitID, config frame.UseConfig) (*frame.Use, error) {
		t.Helper()
		plan, err := host.PlanUnit(ctx, "", refs[id], nil)
		if err != nil {
			t.Fatal(err)
		}
		return host.Acquire(ctx, plan, config)
	}
	for _, id := range []frame.UnitID{"provider", "consumer"} {
		if use, err := acquire(id, frame.UseConfig{Owner: setups["provider"].Lifetime(), Scope: scope}); !errors.Is(err, frame.ErrDependencyCycle) || use != nil {
			t.Fatalf("owner cycle accepted for %s: use=%v error=%v", id, use, err)
		}
	}
	parent, err := acquire("independent", frame.UseConfig{Owner: setups["provider"].Lifetime(), Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if use, err := acquire("consumer", frame.UseConfig{Owner: parent.Lifetime(), Parent: parent}); !errors.Is(err, frame.ErrDependencyCycle) || use != nil {
		t.Fatalf("nested owner cycle accepted: use=%v error=%v", use, err)
	}
	if _, err := parent.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, actualOwner := range []*frame.Lifetime{owner, setups["consumer"].Lifetime()} {
		use, err := acquire("provider", frame.UseConfig{Owner: actualOwner, Scope: scope})
		if err != nil {
			t.Fatalf("acyclic consumer ownership rejected: %v", err)
		}
		if _, err := use.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []frame.UnitID{"consumer", "provider", "independent"} {
		if err := host.RetireInstance(ctx, setups[id].InstanceID()); err != nil {
			t.Fatal(err)
		}
		if err := host.WaitInstanceRetired(ctx, setups[id].InstanceID()); err != nil {
			t.Fatalf("rejected Use retained %s: %v", id, err)
		}
	}
}
