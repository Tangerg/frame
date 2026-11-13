package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestUseOwnerRejectsForeignDependencyAuthority(t *testing.T) {
	ctx := testContext(t)
	first := testHost(t, framehost.Config{})
	second := testHost(t, framehost.Config{})
	var setup *framehost.Setup
	firstPlugin := installNative(t, first, "first", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, original *framehost.Setup) ([]framehost.ExportValue, error) {
			setup = original
			return nil, nil
		},
	})
	secondPlugin := installNative(t, second, "second", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup:       func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) { return nil, nil },
	})
	firstRoot := framehost.UnitRef{InstallationID: "first", Release: firstPlugin.Revision(), UnitID: "main"}
	secondRoot := framehost.UnitRef{InstallationID: "second", Release: secondPlugin.Revision(), UnitID: "main"}
	owner, firstScope := ownerAndScope(t, first)
	_, secondScope := ownerAndScope(t, second)
	firstUse, err := first.AcquireUnit(ctx, firstRoot, framehost.UseConfig{Owner: owner, Scope: firstScope})
	if err != nil {
		t.Fatal(err)
	}
	secondUse, err := second.AcquireUnit(ctx, secondRoot, framehost.UseConfig{Owner: owner, Scope: secondScope})
	if err != nil {
		t.Fatalf("external owner could not compose independent Hosts: %v", err)
	}
	for _, foreign := range []frame.Lifetime{setup.Lifetime(), firstUse.Lifetime()} {
		use, err := second.AcquireUnit(ctx, secondRoot, framehost.UseConfig{Owner: foreign, Scope: secondScope})
		if use != nil {
			_, _ = use.Close(ctx)
		}
		if !errors.Is(err, frame.ErrInvalid) || use != nil {
			t.Fatalf("foreign Host lifetime became a Use owner: %v, %v", use, err)
		}
	}
	for _, use := range []*framehost.Use{firstUse, secondUse} {
		if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
			t.Fatalf("external owner Use did not release: %+v, %v", outcome, err)
		}
	}
}

func TestHostRejectsForeignDependencyLifetime(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	var lifetime frame.Lifetime
	plugin := installNative(t, host, "owner", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
			lifetime = setup.Lifetime()
			return nil, nil
		},
	})
	if _, err := host.ActivateUnit(ctx, "", framehost.UnitRef{InstallationID: "owner", Release: plugin.Revision(), UnitID: "main"}); err != nil {
		t.Fatal(err)
	}
	foreign, err := framehost.New(ctx, framehost.Config{Lifetime: lifetime})
	if foreign.Valid() {
		_, _ = foreign.Close(ctx)
	}
	if !errors.Is(err, frame.ErrInvalid) || foreign.Valid() {
		t.Fatalf("Host accepted another authority's dependency lifetime: %v, %v", foreign, err)
	}
}
