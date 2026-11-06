package frame_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

func TestRunningUnitCopiesShareOneStartClaim(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(ctx, "worker-owner")
	if err != nil {
		t.Fatal(err)
	}
	root := frame.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	first, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	copied := *first
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
