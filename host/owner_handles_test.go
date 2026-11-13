package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type missingHostContext struct{ context.Context }

func TestHostWaitersRejectMissingContexts(t *testing.T) {
	host, err := framehost.New(t.Context(), framehost.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	for _, ctx := range []context.Context{nil, (*missingHostContext)(nil)} {
		if err := host.WaitRetired(ctx, "installation"); !errors.Is(err, frame.ErrInvalid) {
			t.Errorf("retirement wait admitted missing context: %v", err)
		}
		if err := host.WaitInstanceRetired(ctx, "instance"); !errors.Is(err, frame.ErrInvalid) {
			t.Errorf("instance wait admitted missing context: %v", err)
		}
	}
	if _, err := (framehost.Host{}).Close(t.Context()); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("unissued Host admitted close: %v", err)
	}
}

func TestAdmissionCopiesFenceTheOriginalScopeAuthority(t *testing.T) {
	original := framehost.NewAdmission()
	copied := original
	grant, err := copied.OpenScope(t.Context(), "original")
	if err != nil {
		t.Fatal(err)
	}
	if err := original.ValidateScope(grant); err != nil {
		t.Fatalf("copy issued another authority: %v", err)
	}
	original.FenceScope("original")
	if err := copied.ValidateScope(grant); !errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("copy bypassed original fence: %v", err)
	}
	copied = framehost.Admission{}
	if copied.Valid() || !original.Valid() {
		t.Fatal("handle replacement changed the original authority")
	}
}

func TestHostCopiesAdvanceOneInstanceGenerationOwner(t *testing.T) {
	original, err := framehost.New(context.Background(), framehost.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close(context.Background())
	copied := original
	units := make([]framehost.Unit, 0, 2)
	for _, id := range []framehost.UnitID{"first", "second"} {
		unit, err := framehost.DefineUnit(framehost.UnitSpec{ID: id, Prepare: func(*framehost.Setup) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, unit)
	}
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("owner handles")), Units: units})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := original.Install(t.Context(), framehost.InstallationConfig{ID: "owner", Plugin: plugin, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := original.ActivateUnit(t.Context(), "", installed.Unit("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := copied.ActivateUnit(t.Context(), "", installed.Unit("second")); err != nil {
		t.Fatal(err)
	}
	observations := original.Observations()
	if len(observations) != 2 || observations[0].Generation == observations[1].Generation {
		t.Fatalf("copy originated another generation owner: %#v", observations)
	}
	copied = framehost.Host{}
	if copied.Valid() || len(original.Observations()) != 2 {
		t.Fatal("handle replacement changed the original Host")
	}
}

func TestHostCapturesTheIssuedApplicationLifetime(t *testing.T) {
	owner := frame.NewLifetime(context.Background())
	original := owner
	host, err := framehost.New(t.Context(), framehost.Config{Lifetime: owner})
	if err != nil {
		t.Fatal(err)
	}
	owner = frame.Lifetime{}
	if owner.Valid() {
		t.Fatal("replacement handle stayed issued")
	}
	if _, err := host.Close((*missingHostContext)(nil)); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("missing wait context admitted: %v", err)
	}
	if original.IsClosing() {
		t.Fatal("invalid close advanced the original Host lifetime")
	}
	if outcome, err := host.Close(t.Context()); err != nil || !outcome.Released {
		t.Fatal(outcome, err)
	}
	if !original.IsClosing() {
		t.Fatal("Host lost its original lifetime after caller replacement")
	}
}
