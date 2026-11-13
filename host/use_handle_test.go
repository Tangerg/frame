package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestUseCopiesShareOriginalAdmissionAndRetirement(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	plugin := installNative(t, host, "use-handle", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			return nil, nil
		},
	})
	owner, scope := ownerAndScope(t, host)
	root := framehost.UnitRef{InstallationID: "use-handle", Release: plugin.Revision(), UnitID: "main"}
	plan, err := host.PlanUnit(ctx, "", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	use, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	copied := *use
	if err := host.ValidateUse(ctx, &copied); err != nil {
		t.Fatalf("copy lost its original admission: %v", err)
	}
	*use = framehost.Use{}
	if err := host.ValidateUse(ctx, use); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("zero handle retained admission: %v", err)
	}
	if _, err := use.Close(ctx); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("zero handle closed an owner: %v", err)
	}
	child, err := host.AcquireChild(ctx, &copied, plan)
	if err != nil {
		t.Fatalf("copy could not admit a child: %v", err)
	}
	if outcome, err := copied.Close(ctx); err != nil || !outcome.Released {
		t.Fatalf("original retirement: %+v, %v", outcome, err)
	}
	if outcome, err := child.WaitRetired(ctx); err != nil || !outcome.Released {
		t.Fatalf("child outlived its original parent: %+v, %v", outcome, err)
	}
	if err := host.ValidateUse(ctx, &copied); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("retired copy retained admission: %v", err)
	}
	if copied.ID() == "" || !copied.Lifetime().Valid() {
		t.Fatal("retired copy lost its immutable identity")
	}
}

func TestChildAdmissionCapturesOriginalParentBeforePreparation(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	var parent *framehost.Use
	plugin := installNative(t, host, "use-parent", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "parent", Profile: framehost.NativeGo},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			return nil, nil
		},
	}, framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "child", Profile: framehost.NativeGo},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			*parent = framehost.Use{}
			return nil, nil
		},
	})
	owner, scope := ownerAndScope(t, host)
	root := framehost.UnitRef{InstallationID: "use-parent", Release: plugin.Revision(), UnitID: "parent"}
	var err error
	parent, err = host.AcquireUnit(ctx, root, framehost.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	original := *parent
	root.UnitID = "child"
	plan, err := host.PlanUnit(ctx, "", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := host.AcquireChild(ctx, parent, plan)
	if err != nil {
		t.Fatalf("preparation replaced the admitted parent: %v", err)
	}
	if parent.ID() != "" {
		t.Fatal("preparation did not replace the caller's handle")
	}
	if err := host.ValidateUse(ctx, &original); err != nil {
		t.Fatalf("preparation replaced the original parent owner: %v", err)
	}
	if outcome, err := original.Close(ctx); err != nil || !outcome.Released {
		t.Fatalf("parent close: %+v, %v", outcome, err)
	}
	if outcome, err := child.WaitRetired(ctx); err != nil || !outcome.Released {
		t.Fatalf("child retirement: %+v, %v", outcome, err)
	}
}
