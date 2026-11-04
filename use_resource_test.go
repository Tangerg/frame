package frame_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

func TestHostOwnedUseConsumesOneResource(t *testing.T) {
	ctx := testContext(t)
	lifetime, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxResources: 2})
	if err != nil {
		t.Fatal(err)
	}
	host := testHost(t, frame.HostConfig{Lifetime: lifetime})
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(ctx, "caller")
	if err != nil {
		t.Fatal(err)
	}
	running, err := host.StartUnit(ctx, grant, frame.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"})
	if err != nil {
		t.Fatalf("one instance and its host-owned use exceeded two resources: %v", err)
	}
	if err := running.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if cleanups.Load() != 1 {
		t.Fatalf("original instance closed %d times", cleanups.Load())
	}
}
