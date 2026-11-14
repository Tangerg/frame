package host_test

import (
	"context"
	"sync/atomic"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

func TestRunningUnitCopiesShareOneStartClaim(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(ctx, "worker-owner")
	if err != nil {
		t.Fatal(err)
	}
	root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	first, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	copied := first
	stopped := make(chan error, 2)
	go func() { stopped <- first.Stop(ctx) }()
	go func() { stopped <- copied.Stop(ctx) }()
	for range cap(stopped) {
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
	}
	if first.ID() != copied.ID() || first.ID() != second.ID() || cleanups.Load() != 0 {
		t.Fatalf("copies consumed an independent start claim: first=%s, copy=%s, second=%s, cleanups=%d", first.ID(), copied.ID(), second.ID(), cleanups.Load())
	}
	third, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatalf("copied stop withdrew the shared instance: %v", err)
	}
	if third.ID() != second.ID() {
		t.Fatal("copy stopping replaced the original instance")
	}
	if err := second.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if cleanups.Load() != 0 {
		t.Fatal("an independent claim stopped the remaining client")
	}
	if err := third.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stoppedCopy := copied
	if err := stoppedCopy.Stop(ctx); err != nil || cleanups.Load() != 1 {
		t.Fatalf("original retirement changed: cleanups=%d, error=%v", cleanups.Load(), err)
	}
}

func TestOriginalStartClaimsCannotWithdrawSuccessor(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(ctx, "worker-owner")
	if err != nil {
		t.Fatal(err)
	}
	root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	original, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.RetireInstance(ctx, original.ID()); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitInstanceRetired(ctx, original.ID()); err != nil {
		t.Fatal(err)
	}
	successor, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	if successor.ID() == original.ID() || other.ID() != original.ID() || cleanups.Load() != 1 {
		t.Fatal("successor did not follow the original instance's confirmed exit")
	}
	for _, claim := range []framehost.RunningUnit{original, other} {
		if err := claim.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	current, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID() != successor.ID() || cleanups.Load() != 1 {
		t.Fatal("an original start claim withdrew its successor")
	}
	for _, claim := range []framehost.RunningUnit{current, successor} {
		if err := claim.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if cleanups.Load() != 2 {
		t.Fatalf("original and successor cleanup count = %d", cleanups.Load())
	}
}
