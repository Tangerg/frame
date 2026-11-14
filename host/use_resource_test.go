package host_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestHostOwnedUseConsumesOneResource(t *testing.T) {
	ctx := testContext(t)
	lifetime, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxResources: 2})
	if err != nil {
		t.Fatal(err)
	}
	host := testHost(t, framehost.Config{Lifetime: lifetime})
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(ctx, "caller")
	if err != nil {
		t.Fatal(err)
	}
	use, err := host.AcquireUnit(ctx, framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: lifetime, Scope: grant})
	if err != nil {
		t.Fatalf("one instance and its host-owned use exceeded two resources: %v", err)
	}
	if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
		t.Fatalf("host-owned use did not release: %+v, %v", outcome, err)
	}
	if err := host.Retire(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitRetired(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if cleanups.Load() != 1 {
		t.Fatalf("original instance closed %d times", cleanups.Load())
	}
}
