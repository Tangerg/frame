package frame_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

func TestScopedStartDoesNotAdoptInstallationScopedInstance(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, frame.HostConfig{})
	prepared := make(chan frame.ScopeGrant, 2)
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			prepared <- setup.ScopeGrant()
			_, err := setup.Cleanup("worker", func() error { cleanups.Add(1); return nil })
			return nil, err
		},
	})
	root := frame.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"}
	if _, err := host.ActivateUnit(ctx, "", root); err != nil {
		t.Fatal(err)
	}
	original := <-prepared
	grant, err := host.Admission().OpenScope(ctx, "scoped-worker")
	if err != nil {
		t.Fatal(err)
	}
	if running, err := host.StartUnit(ctx, grant, root); running != nil || !errors.Is(err, frame.ErrFenced) {
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
	host := testHost(t, frame.HostConfig{})
	service, err := frame.NewContract[string]("shared.text", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	var providerPrepares, providerCleanups atomic.Int32
	provider := installNative(t, host, "provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "text", Contract: service.Ref()}}},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			providerPrepares.Add(1)
			if _, err := setup.Cleanup("provider", func() error { providerCleanups.Add(1); return nil }); err != nil {
				return nil, err
			}
			return []frame.ExportValue{frame.Provide("text", service, "shared")}, nil
		},
	})
	providerRef := source("provider", provider, "main", "text", service.Ref())
	if _, err := host.Activate(ctx, providerRef); err != nil {
		t.Fatal(err)
	}
	var running []*frame.RunningUnit
	var grants []frame.ScopeGrant
	var roots []frame.UnitRef
	for _, id := range []string{"consumer-one", "consumer-two"} {
		grant, err := host.Admission().OpenScope(ctx, frame.ScopeID(id))
		if err != nil {
			t.Fatal(err)
		}
		consumer := installNative(t, host, id, frame.Unit{
			Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Imports: []frame.ImportDeclaration{{Slot: "text", Contract: service.Ref(), Providers: []frame.ExportRef{providerRef}}}},
			Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
				if setup.ScopeGrant() != grant {
					return nil, errors.New("consumer prepared under another grant")
				}
				text, err := frame.Import(setup, "text", service)
				if err != nil {
					return nil, err
				}
				if text != "shared" {
					return nil, errors.New("consumer received another provider")
				}
				return nil, nil
			},
		})
		root := frame.UnitRef{InstallationID: frame.InstallationID(id), Release: consumer.Revision(), UnitID: "main"}
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
	host := testHost(t, frame.HostConfig{})
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	plugin := installNative(t, host, "preparing", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			close(entered)
			<-release
			return nil, nil
		},
	})
	root := frame.UnitRef{InstallationID: "preparing", Release: plugin.Revision(), UnitID: "main"}
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
	if running, err := host.StartUnit(ctx, grant, root); running != nil || !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("scoped start waited for another authority's plugin preparation: %+v, %v", running, err)
	}
	unblock()
	if err := <-activated; err != nil {
		t.Fatalf("refused start disturbed installation activation: %v", err)
	}
}
