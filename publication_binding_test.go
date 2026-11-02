package frame_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
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
			business := &retainedUseOwner{read: func(_ context.Context, id frame.InstallationID) ([]frame.Dependency, error) {
				if retained == nil {
					return nil, nil
				}
				binding, err := frame.ParseBinding(retained)
				if err != nil {
					return nil, err
				}
				var dependencies []frame.Dependency
				for _, dependency := range binding.Dependencies() {
					if dependency.InstallationID == id {
						dependencies = append(dependencies, dependency)
					}
				}
				return dependencies, nil
			}}
			host := testHost(t, frame.HostConfig{UseOwners: []frame.UseOwner{business}})
			point, _ := frame.NewPoint[string, string]("example.binding.inputs", "1")
			service, _ := frame.NewContract[string]("example.binding.output", "1")
			if err := host.Register(point, service); err != nil {
				t.Fatal(err)
			}
			var providerID string
			provider := installNative(t, host, "provider", frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "input", Contract: point.Ref()}}},
				Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
					providerID = setup.InstanceID()
					return []frame.ExportValue{frame.Contribute("input", point, "original", "original-value")}, nil
				},
			})
			providerSource := source("provider", provider, "main", "input", point.Ref())
			if _, err := host.Activate(ctx, providerSource); err != nil {
				t.Fatal(err)
			}
			var setups atomic.Int64
			plugins := []*frame.Plugin{provider}
			root := providerSource
			for _, id := range []string{"consumer", "downstream"}[:depth] {
				dependency := root
				consumer := installNative(t, host, id, frame.Unit{
					Declaration: frame.UnitDeclaration{
						ID: "main", Profile: frame.NativeGo,
						Imports: []frame.ImportDeclaration{{Slot: "input", Contract: dependency.Contract, Providers: []frame.ExportRef{dependency}}},
						Exports: []frame.ExportDeclaration{{Name: "output", Contract: service.Ref()}},
					},
					Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
						setups.Add(1)
						inputs, err := setup.Imports("input")
						if err != nil {
							return nil, err
						}
						return []frame.ExportValue{frame.Provide("output", service, inputs[0].Value.(string))}, nil
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
			candidates, err := frame.Candidates(snapshot, point)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("original candidate: %v, %v", candidates, err)
			}
			if err := host.Withdraw(ctx, candidates[0].ID()); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Publish(ctx, providerID, "replacement", frame.Contribute("input", point, "dynamic", "dynamic-value")); err != nil {
				t.Fatal(err)
			}
			err = host.Admission().Admit(ctx, func(admitted context.Context) error {
				_, err := host.ValidatePlanUnder(admitted, plan)
				return err
			})
			if !errors.Is(err, frame.ErrUnavailable) {
				t.Fatalf("dynamic replacement entered durable admission: %v", err)
			}
			use, err := host.Acquire(ctx, plan, frame.UseConfig{Owner: owner, Scope: grant})
			if !errors.Is(err, frame.ErrUnavailable) || use != nil || setups.Load() != 0 {
				t.Fatalf("dynamic replacement reached setup: use=%v, err=%v, setups=%d", use, err, setups.Load())
			}
			var records []frame.InstallationRecord
			for _, id := range []frame.InstallationID{"provider", "consumer", "downstream"}[:depth+1] {
				record, _ := host.Installation(id)
				records = append(records, record)
			}
			if _, err := owner.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Close(ctx); err != nil {
				t.Fatal(err)
			}
			admission := frame.NewAdmission()
			restored := testHost(t, frame.HostConfig{
				Admission: admission, UseOwners: []frame.UseOwner{business},
				ReadAcceptedBinding: func(admitted context.Context, id string, scope frame.ScopeGrant) (frame.BindingSnapshot, error) {
					if err := admission.CheckAdmitted(admitted); err != nil {
						return frame.BindingSnapshot{}, err
					}
					if id != "original-record" || scope.ID() != grant.ID() {
						return frame.BindingSnapshot{}, frame.ErrUnavailable
					}
					return frame.ParseBinding(retained)
				},
			})
			if err := restored.Register(point, service); err != nil {
				t.Fatal(err)
			}
			if err := restored.Restore(ctx, records, plugins); err != nil {
				t.Fatal(err)
			}
			newOwner, newScope := ownerAndScope(t, restored)
			recovered, err := restored.AcquireRecord(ctx, "original-record", frame.UseConfig{Owner: newOwner, Scope: newScope})
			if err != nil {
				t.Fatal(err)
			}
			if err := frame.BorrowExport(ctx, recovered, root, func(_ context.Context, value string) error {
				if value != "original-value" {
					t.Fatalf("reconstructed setup used %q", value)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			binding, err := frame.ParseBinding(retained)
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
	host := testHost(t, frame.HostConfig{})
	point, _ := frame.NewPoint[string, string]("example.slow.inputs", "1")
	service, _ := frame.NewContract[string]("example.slow.output", "1")
	if err := host.Register(point, service); err != nil {
		t.Fatal(err)
	}
	var providerID string
	provider := installNative(t, host, "provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "input", Contract: point.Ref()}}},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			providerID = setup.InstanceID()
			return []frame.ExportValue{frame.Contribute("input", point, "original", "original-value")}, nil
		},
	})
	dependency := source("provider", provider, "main", "input", point.Ref())
	entered, resume := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(resume) })
	defer unblock()
	consumer := installNative(t, host, "consumer", frame.Unit{
		Declaration: frame.UnitDeclaration{
			ID: "main", Profile: frame.NativeGo,
			Imports: []frame.ImportDeclaration{{Slot: "input", Contract: point.Ref(), Providers: []frame.ExportRef{dependency}}},
			Exports: []frame.ExportDeclaration{{Name: "output", Contract: service.Ref()}},
		},
		Setup: func(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
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
			return []frame.ExportValue{frame.Provide("output", service, inputs[0].Value.(string))}, nil
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
	candidates, err := frame.Candidates(snapshot, point)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("original candidate: %v, %v", candidates, err)
	}
	if err := host.Withdraw(ctx, candidates[0].ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Publish(ctx, providerID, "replacement", frame.Contribute("input", point, "dynamic", "dynamic-value")); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-finished; !errors.Is(err, frame.ErrUnavailable) {
		t.Fatalf("slow setup outlived its original export admission: %v", err)
	}
}
