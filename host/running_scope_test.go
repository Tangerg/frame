package host_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestScopedRetirementKeepsOriginalCancellationCause(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	notified := make(chan error, 1)
	var cleanups atomic.Int32
	plugin := installNative(t, host, "original-cause", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			_, err := setup.Own(frame.Resource{
				Name:   "original execution",
				Cancel: func(cause error) { notified <- cause },
				Close: func(context.Context) (frame.ReleaseState, error) {
					cleanups.Add(1)
					return frame.ResourceReleased, nil
				},
			})
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(ctx, "original-start")
	if err != nil {
		t.Fatal(err)
	}
	running, err := host.StartUnit(ctx, grant, framehost.UnitRef{InstallationID: "original-cause", Release: plugin.Revision(), UnitID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	host.Admission().FenceScope(grant.ID())
	if err := host.WaitInstanceRetired(ctx, running.ID()); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-notified:
		if !errors.Is(cause, framehost.ErrFenced) {
			t.Fatalf("instance retirement replaced its original fence: %v", cause)
		}
	default:
		t.Fatal("original resource received no cancellation")
	}
	if err := running.Stop(ctx); err != nil || cleanups.Load() != 1 {
		t.Fatalf("original retirement changed its physical exit: cleanups=%d error=%v", cleanups.Load(), err)
	}
}

func TestScopedStartDoesNotAdoptInstallationScopedInstance(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	prepared := make(chan framehost.ScopeGrant, 2)
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			prepared <- setup.ScopeGrant()
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	root := framehost.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	if _, err := host.ActivateUnit(ctx, "", root); err != nil {
		t.Fatal(err)
	}
	original := <-prepared
	grant, err := host.Admission().OpenScope(ctx, "scoped-worker")
	if err != nil {
		t.Fatal(err)
	}
	if running, err := host.StartUnit(ctx, grant, root); running.Valid() || !errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("installation-scoped runtime was adopted by another grant: %+v, %v", running, err)
	}
	host.Admission().FenceScope(grant.ID())
	if cleanups.Load() != 0 || original == grant {
		t.Fatal("refused start changed the original installation authority")
	}
	if _, err := host.ActivateUnit(ctx, "", root); err != nil || len(prepared) != 0 {
		t.Fatalf("refused scoped start replaced the shared instance: %v", err)
	}
	if err := host.Retire(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitRetired(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	grant, err = host.Admission().OpenScope(ctx, "scoped-worker")
	if err != nil {
		t.Fatal(err)
	}
	running, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		t.Fatal(err)
	}
	if actual := <-prepared; actual != grant {
		t.Fatalf("new scoped instance used another authority: got=%s, want=%s", actual.ID(), grant.ID())
	}
	host.Admission().FenceScope(grant.ID())
	if err := host.WaitInstanceRetired(ctx, running.ID()); err != nil {
		t.Fatalf("original grant did not retire its instance: %v", err)
	}
	if err := running.Stop(ctx); err != nil || cleanups.Load() != 2 {
		t.Fatalf("original scoped retirement changed: cleanups=%d, error=%v", cleanups.Load(), err)
	}
}

func TestScopedStartKeepsInstallationScopedProviderAlive(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	service, err := framehost.NewContract[string]("shared.text", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	var providerPrepares, providerCleanups atomic.Int32
	provider := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "text", Contract: service.Ref()}}},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			providerPrepares.Add(1)
			if _, err := setup.Cleanup("provider", func() error { providerCleanups.Add(1); return nil }); err != nil {
				return nil, err
			}
			return []framehost.ExportValue{framehost.Provide("text", service, "shared")}, nil
		},
	})
	providerRef := source("provider", provider, "main", "text", service.Ref())
	if _, err := host.Activate(ctx, providerRef); err != nil {
		t.Fatal(err)
	}
	var running []framehost.RunningUnit
	var grants []framehost.ScopeGrant
	var roots []framehost.UnitRef
	for _, id := range []string{"consumer-one", "consumer-two"} {
		grant, err := host.Admission().OpenScope(ctx, framehost.ScopeID(id))
		if err != nil {
			t.Fatal(err)
		}
		consumer := installNative(t, host, id, framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "text", Contract: service.Ref()}}, Fixed: []framehost.SlotProviders{{Slot: "text", Providers: []framehost.ExportRef{providerRef}}}},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				if setup.ScopeGrant() != grant {
					return nil, errors.New("consumer prepared under another grant")
				}
				text, err := framehost.Import(setup, "text", service)
				if err != nil {
					return nil, err
				}
				if text != "shared" {
					return nil, errors.New("consumer received another provider")
				}
				return nil, nil
			},
		})
		root := framehost.UnitRef{InstallationID: framehost.InstallationID(id), Release: consumer.Revision(), UnitID: "main"}
		handle, err := host.StartUnit(ctx, grant, root)
		if err != nil {
			t.Fatal(err)
		}
		running = append(running, handle)
		grants = append(grants, grant)
		roots = append(roots, root)
	}
	for i, grant := range grants {
		host.Admission().FenceScope(grant.ID())
		if err := host.WaitInstanceRetired(ctx, running[i].ID()); err != nil {
			t.Fatal(err)
		}
		if err := running[i].Stop(ctx); err != nil {
			t.Fatal(err)
		}
		for j := i + 1; j < len(running); j++ {
			additional, err := host.StartUnit(ctx, grants[j], roots[j])
			if err != nil {
				t.Fatalf("revocation stopped another consumer: %v", err)
			}
			if additional.ID() != running[j].ID() {
				t.Fatal("revocation replaced another consumer's instance")
			}
			if err := additional.Stop(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := host.Activate(ctx, providerRef); err != nil || providerPrepares.Load() != 1 || providerCleanups.Load() != 0 {
			t.Fatalf("consumer revocation changed its shared provider: prepares=%d, cleanups=%d, error=%v", providerPrepares.Load(), providerCleanups.Load(), err)
		}
	}
}

func TestScopedStartRefusesInstallationScopedPreparationBeforeWaiting(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	plugin := installNative(t, host, "preparing", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			close(entered)
			<-release
			return nil, nil
		},
	})
	root := framehost.UnitRef{InstallationID: "preparing", Release: plugin.Revision(), UnitID: "main"}
	activated := make(chan error, 1)
	go func() {
		_, err := host.ActivateUnit(ctx, "", root)
		activated <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	grant, err := host.Admission().OpenScope(ctx, "scoped-start")
	if err != nil {
		t.Fatal(err)
	}
	if running, err := host.StartUnit(ctx, grant, root); running.Valid() || !errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("scoped start waited for another authority's plugin preparation: %+v, %v", running, err)
	}
	unblock()
	if err := <-activated; err != nil {
		t.Fatalf("refused start disturbed installation activation: %v", err)
	}
}
