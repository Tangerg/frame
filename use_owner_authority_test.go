package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestUseOwnerRejectsForeignDependencyAuthority(t *testing.T) {
	ctx := testContext(t)
	first := testHost(t, frame.HostConfig{})
	second := testHost(t, frame.HostConfig{})
	var setup *frame.Setup
	firstPlugin := installNative(t, first, "first", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup: func(_ context.Context, original *frame.Setup) ([]frame.ExportValue, error) {
			setup = original
			return nil, nil
		},
	})
	secondPlugin := installNative(t, second, "second", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup:       func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil },
	})
	firstRoot := frame.UnitRef{InstallationID: "first", Release: firstPlugin.Revision(), UnitID: "main"}
	secondRoot := frame.UnitRef{InstallationID: "second", Release: secondPlugin.Revision(), UnitID: "main"}
	owner, firstScope := ownerAndScope(t, first)
	_, secondScope := ownerAndScope(t, second)
	firstUse, err := first.AcquireUnit(ctx, firstRoot, frame.UseConfig{Owner: owner, Scope: firstScope})
	if err != nil {
		t.Fatal(err)
	}
	secondUse, err := second.AcquireUnit(ctx, secondRoot, frame.UseConfig{Owner: owner, Scope: secondScope})
	if err != nil {
		t.Fatalf("external owner could not compose independent Hosts: %v", err)
	}
	for _, foreign := range []*frame.Lifetime{setup.Lifetime(), firstUse.Lifetime()} {
		use, err := second.AcquireUnit(ctx, secondRoot, frame.UseConfig{Owner: foreign, Scope: secondScope})
		if use != nil {
			_, _ = use.Close(ctx)
		}
		if !errors.Is(err, frame.ErrInvalid) || use != nil {
			t.Fatalf("foreign Host lifetime became a Use owner: %v, %v", use, err)
		}
	}
	for _, use := range []*frame.Use{firstUse, secondUse} {
		if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
			t.Fatalf("external owner Use did not release: %+v, %v", outcome, err)
		}
	}
}

func TestHostRejectsForeignDependencyLifetime(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	var lifetime *frame.Lifetime
	plugin := installNative(t, host, "owner", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			lifetime = setup.Lifetime()
			return nil, nil
		},
	})
	if _, err := host.ActivateUnit(ctx, "", frame.UnitRef{InstallationID: "owner", Release: plugin.Revision(), UnitID: "main"}); err != nil {
		t.Fatal(err)
	}
	foreign, err := frame.NewHost(ctx, frame.HostConfig{Lifetime: lifetime})
	if foreign != nil {
		_, _ = foreign.Close(ctx)
	}
	if !errors.Is(err, frame.ErrInvalid) || foreign != nil {
		t.Fatalf("Host accepted another authority's dependency lifetime: %v, %v", foreign, err)
	}
}
