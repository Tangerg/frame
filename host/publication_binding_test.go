package host_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

func TestDynamicPublicationCannotReplaceDeclaredSetup(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		depth int
	}{{"direct", 1}, {"transitive", 2}} {
		t.Run(scenario.name, func(t *testing.T) {
			depth := scenario.depth
			ctx := testContext(t)
			var retained []byte
			business := &retainedUseOwner{read: func(_ context.Context, id framehost.InstallationID) ([]framehost.Dependency, error) {
				if retained == nil {
					return nil, nil
				}
				binding, err := framehost.ParseBinding(retained)
				if err != nil {
					return nil, err
				}
				var dependencies []framehost.Dependency
				for _, dependency := range binding.Dependencies() {
					if dependency.InstallationID == id {
						dependencies = append(dependencies, dependency)
					}
				}
				return dependencies, nil
			}}
			host := testHost(t, framehost.Config{UseOwners: []framehost.UseOwner{business}})
			point, _ := framehost.NewPoint[string, string]("example.binding.inputs", "1")
			service, _ := framehost.NewContract[string]("example.binding.output", "1")
			if err := host.Register(point, service); err != nil {
				t.Fatal(err)
			}
			var providerID string
			provider := installNative(t, host, "provider", framehost.Unit{
				Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "input", Contract: point.Ref()}}},
				Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
					providerID = setup.InstanceID()
					return []framehost.ExportValue{framehost.Contribute("input", point, "original", "original-value")}, nil
				},
			})
			providerSource := source("provider", provider, "main", "input", point.Ref())
			if _, err := host.Activate(ctx, providerSource); err != nil {
				t.Fatal(err)
			}
			var setups atomic.Int64
			plugins := []*framehost.Plugin{provider}
			root := providerSource
			for _, id := range []string{"consumer", "downstream"}[:depth] {
				dependency := root
				consumer := installNative(t, host, id, framehost.Unit{
					Declaration: framehost.UnitDeclaration{
						ID: "main", Profile: framehost.NativeGo,
						Imports: []framehost.ImportDeclaration{{Slot: "input", Contract: dependency.Contract}}, Fixed: []framehost.SlotProviders{{Slot: "input", Providers: []framehost.ExportRef{dependency}}},
						Exports: []framehost.ExportDeclaration{{Name: "output", Contract: service.Ref()}},
					},
					Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
						setups.Add(1)
						inputs, err := setup.Imports("input")
						if err != nil {
							return nil, err
						}
						return []framehost.ExportValue{framehost.Provide("output", service, inputs[0].Value.(string))}, nil
					},
				})
				plugins = append(plugins, consumer)
				root = source(id, consumer, "main", "output", service.Ref())
			}
			plan, err := host.Plan(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			owner, grant := ownerAndScope(t, host)
			if err := host.Admission().Admit(ctx, func(admitted context.Context) error {
				if err := host.Admission().ValidateScope(grant); err != nil {
					return err
				}
				binding, err := host.ValidatePlanUnder(admitted, plan)
				if err != nil {
					return err
				}
				retained, err = binding.CanonicalJSON()
				return err
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := host.Snapshot(ctx, point.Ref())
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := framehost.Candidates(snapshot, point)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("original candidate: %v, %v", candidates, err)
			}
			if err := host.Withdraw(ctx, candidates[0].ID()); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Publish(ctx, "replacement", preparePublication(t, host, providerID, framehost.Contribute("input", point, "dynamic", "dynamic-value"))); err != nil {
				t.Fatal(err)
			}
			err = host.Admission().Admit(ctx, func(admitted context.Context) error {
				_, err := host.ValidatePlanUnder(admitted, plan)
				return err
			})
			if !errors.Is(err, framehost.ErrUnavailable) {
				t.Fatalf("dynamic replacement entered durable admission: %v", err)
			}
			use, err := host.Acquire(ctx, plan, framehost.UseConfig{Owner: owner, Scope: grant})
			if !errors.Is(err, framehost.ErrUnavailable) || use.Valid() || setups.Load() != 0 {
				t.Fatalf("dynamic replacement reached setup: use=%v, err=%v, setups=%d", use, err, setups.Load())
			}
			var records []framehost.InstallationRecord
			for _, id := range []framehost.InstallationID{"provider", "consumer", "downstream"}[:depth+1] {
				record, _ := host.Installation(id)
				records = append(records, record)
			}
			if _, err := owner.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Close(ctx); err != nil {
				t.Fatal(err)
			}
			admission := framehost.NewAdmission()
			restored := testHost(t, framehost.Config{
				Authority: admission, UseOwners: []framehost.UseOwner{business},
				ReadAcceptedBinding: func(admitted context.Context, id string, scope framehost.ScopeGrant) (framehost.BindingSnapshot, error) {
					if err := admission.CheckAdmitted(admitted); err != nil {
						return framehost.BindingSnapshot{}, err
					}
					if id != "original-record" || scope.ID() != grant.ID() {
						return framehost.BindingSnapshot{}, framehost.ErrUnavailable
					}
					return framehost.ParseBinding(retained)
				},
			})
			if err := restored.Register(point, service); err != nil {
				t.Fatal(err)
			}
			if err := restored.Restore(ctx, records, plugins); err != nil {
				t.Fatal(err)
			}
			newOwner, newScope := ownerAndScope(t, restored)
			recovered, err := restored.AcquireRecord(ctx, "original-record", framehost.UseConfig{Owner: newOwner, Scope: newScope})
			if err != nil {
				t.Fatal(err)
			}
			if err := framehost.BorrowExport(ctx, recovered, root, func(_ context.Context, value string) error {
				if value != "original-value" {
					t.Fatalf("reconstructed setup used %q", value)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			binding, err := framehost.ParseBinding(retained)
			if err != nil {
				t.Fatal(err)
			}
			originalID, _ := binding.Identity()
			restoredID, _ := recovered.Binding().Identity()
			if originalID != restoredID || setups.Load() != int64(depth) {
				t.Fatalf("reconstruction changed the original graph: identity=%v, setups=%d", originalID == restoredID, setups.Load())
			}
		})
	}
}

func TestSetupRechecksOriginalExportAfterSlowPreparation(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	point, _ := framehost.NewPoint[string, string]("example.slow.inputs", "1")
	service, _ := framehost.NewContract[string]("example.slow.output", "1")
	if err := host.Register(point, service); err != nil {
		t.Fatal(err)
	}
	var providerID string
	provider := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "input", Contract: point.Ref()}}},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			providerID = setup.InstanceID()
			return []framehost.ExportValue{framehost.Contribute("input", point, "original", "original-value")}, nil
		},
	})
	dependency := source("provider", provider, "main", "input", point.Ref())
	entered, resume := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(resume) })
	defer unblock()
	consumer := installNative(t, host, "consumer", framehost.Unit{
		Declaration: framehost.UnitDeclaration{
			ID: "main", Profile: framehost.NativeGo,
			Imports: []framehost.ImportDeclaration{{Slot: "input", Contract: point.Ref()}}, Fixed: []framehost.SlotProviders{{Slot: "input", Providers: []framehost.ExportRef{dependency}}},
			Exports: []framehost.ExportDeclaration{{Name: "output", Contract: service.Ref()}},
		},
		Setup: func(ctx context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			inputs, err := setup.Imports("input")
			if err != nil {
				return nil, err
			}
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []framehost.ExportValue{framehost.Provide("output", service, inputs[0].Value.(string))}, nil
		},
	})
	root := source("consumer", consumer, "main", "output", service.Ref())
	finished := make(chan error, 1)
	go func() { _, err := host.Activate(ctx, root); finished <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("original candidate: %v, %v", candidates, err)
	}
	if err := host.Withdraw(ctx, candidates[0].ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Publish(ctx, "replacement", preparePublication(t, host, providerID, framehost.Contribute("input", point, "dynamic", "dynamic-value"))); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-finished; !errors.Is(err, framehost.ErrUnavailable) {
		t.Fatalf("slow setup outlived its original export admission: %v", err)
	}
}
