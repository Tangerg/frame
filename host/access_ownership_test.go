package host

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

func TestAccessCopiesEndWithOriginalBorrow(t *testing.T) {
	ctx := t.Context()
	host, err := New(ctx, Config{})
	if err != nil {
		t.Fatal(err)
	}
	point, err := NewPoint[string, string]("example.access-copy", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	plugin, err := Define(Definition{Revision: framevalue.Hash([]byte("access-copy")), Units: []Unit{{
		Declaration: UnitDeclaration{ID: "main", Profile: NativeGo, Exports: []ExportDeclaration{{Name: "original", Contract: point.Ref()}}},
		Setup: func(context.Context, *Setup) ([]ExportValue, error) {
			return []ExportValue{Contribute("original", point, "metadata", "implementation")}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, InstallationConfig{ID: "provider", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ActivateUnit(ctx, "", UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := Select(snapshot, point, "original", candidates...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	owner := frame.NewLifetime(ctx)
	grant, err := host.Admission().OpenScope(ctx, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	use, err := host.Acquire(ctx, plan, UseConfig{Owner: owner, Scope: grant})
	if err != nil {
		t.Fatal(err)
	}
	var original *Access
	var copied Access
	if err := withAccess(ctx, use, func(access *Access) error {
		original = access
		copied = *access
		member, err := selection.One(&copied)
		if err != nil || member.Value != "implementation" {
			t.Fatalf("live copied access lost its original member: %+v, %v", member, err)
		}
		saved := *use
		*use = Use{}
		defer func() { *use = saved }()
		member, err = selection.One(access)
		if err != nil || member.Value != "implementation" {
			t.Fatalf("overwriting the user handle changed an admitted access: %+v, %v", member, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, access := range []*Access{original, &copied} {
		if member, err := selection.One(access); !errors.Is(err, frame.ErrClosed) {
			t.Errorf("ended borrow still exposed its member: %+v, %v", member, err)
		}
	}
	if err := host.ValidateUse(ctx, use); err != nil {
		t.Fatalf("ending one borrow closed the surrounding Use: %v", err)
	}
	if _, err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
