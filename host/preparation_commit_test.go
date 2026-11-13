package host

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	framevalue "github.com/Tangerg/frame/value"
)

func TestSetupPublicationAndReadinessCommitTogether(t *testing.T) {
	host, err := New(t.Context(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	contract, err := NewContract[string]("preparation.atomic", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	plugin, err := Define(Definition{Revision: framevalue.Hash([]byte("preparation atomic")), Units: []Unit{{
		Declaration: UnitDeclaration{ID: "main", Profile: NativeGo, Exports: []ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
		Setup:       func(context.Context, *Setup) ([]ExportValue, error) { return nil, nil },
	}}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := host.Install(t.Context(), InstallationConfig{ID: "publisher", Plugin: plugin, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	source := ExportRef{InstallationID: record.ID, Release: record.Release, UnitID: "main", Export: "value", Contract: contract.Ref()}
	plan, err := host.Plan(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	instance, _, err := host.planInstanceLocked(plan.binding, plan.binding.Units[0], "", plan.bindingReads, nil, ScopeGrant{})
	host.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := host.prepareSetupExports(t.Context(), instance, []ExportValue{Provide("value", contract, "implementation")})
	if err == nil {
		host.mu.Lock()
		err = instance.publishSetupLocked(prepared)
		host.mu.Unlock()
	}
	observations := host.Observations()
	if err != nil {
		instance.failPreparation(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].State != Ready {
		t.Fatalf("published setup was observable before readiness committed: %+v", observations)
	}
}

func TestActivationReadsOriginalProviderBeforeCancellationNotification(t *testing.T) {
	host, err := New(t.Context(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	input, err := NewContract[string]("preparation.provider", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(input); err != nil {
		t.Fatal(err)
	}
	revision := framevalue.Hash([]byte("original provider cancellation"))
	providerRef := ExportRef{InstallationID: "graph", Release: revision, UnitID: "provider", Export: "value", Contract: input.Ref()}
	var provider *unitInstance
	var activated atomic.Int32
	plugin, err := Define(Definition{Revision: revision, Units: []Unit{
		{
			Declaration: UnitDeclaration{ID: "provider", Profile: NativeGo, Exports: []ExportDeclaration{{Name: "value", Contract: input.Ref()}}},
			Setup: func(_ context.Context, setup *Setup) ([]ExportValue, error) {
				provider = setup.instance
				return []ExportValue{Provide("value", input, "original")}, nil
			},
		},
		{
			Declaration: UnitDeclaration{ID: "consumer", Profile: NativeGo, Imports: []ImportDeclaration{{Slot: "input", Contract: input.Ref(), Providers: []ExportRef{providerRef}}}},
			Setup: func(_ context.Context, setup *Setup) ([]ExportValue, error) {
				if err := setup.OnActivate(func(context.Context) error { activated.Add(1); return nil }); err != nil {
					return nil, err
				}
				// Delay only the notification; the original cancellation is already a fact.
				host.mu.Lock()
				for _, stop := range provider.stops {
					if !stop() {
						host.mu.Unlock()
						return nil, errors.New("provider notification ran before cancellation")
					}
				}
				provider.lifetime.Cancel(errors.New("original provider stopped"))
				host.mu.Unlock()
				return nil, nil
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := host.Install(t.Context(), InstallationConfig{ID: "graph", Plugin: plugin, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.ActivateUnit(t.Context(), "", record.Unit("consumer")); !errors.Is(err, ErrStaleBinding) || activated.Load() != 0 {
		t.Fatalf("original provider cancellation did not fence activation: activations=%d error=%v", activated.Load(), err)
	}
}
