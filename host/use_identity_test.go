package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestPreparedBindingCannotAdoptSuccessorInstances(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		unitRoot    bool
		changed     InstallationID
		recordID    string
		sameBinding bool
	}{
		{"export/root/plan", false, "root", "", false},
		{"export/root/record", false, "root", "original-binding", false},
		{"export/dependency/plan", false, "dependency", "", false},
		{"export/dependency/record", false, "dependency", "original-binding", false},
		{"unit/root/plan", true, "root", "", false},
		{"unit/root/record", true, "root", "original-binding", false},
		{"unit/dependency/plan", true, "dependency", "", false},
		{"unit/dependency/record", true, "dependency", "original-binding", false},
		{"same graph/export/root/plan", false, "root", "", true},
		{"same graph/export/root/record", false, "root", "original-binding", true},
		{"same graph/export/dependency/plan", false, "dependency", "", true},
		{"same graph/export/dependency/record", false, "dependency", "original-binding", true},
		{"same graph/unit/root/plan", true, "root", "", true},
		{"same graph/unit/root/record", true, "root", "original-binding", true},
		{"same graph/unit/dependency/plan", true, "dependency", "", true},
		{"same graph/unit/dependency/record", true, "dependency", "original-binding", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var accepted BindingSnapshot
			host, err := New(context.Background(), Config{ReadAcceptedBinding: func(context.Context, string, ScopeGrant) (BindingSnapshot, error) {
				return accepted.Clone(), nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			owner := frame.NewLifetime(context.Background())
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if outcome, err := owner.Close(cleanup); err != nil || !outcome.Released {
					t.Errorf("owner close: %+v, %v", outcome, err)
				}
				if outcome, err := host.Close(cleanup); err != nil || !outcome.Released {
					t.Errorf("host close: %+v, %v", outcome, err)
				}
			})
			scope, err := host.Admission().OpenScope(ctx, "identity-consumer")
			if err != nil {
				t.Fatal(err)
			}
			input, err := NewContract[string]("test.identity-input", "1")
			if err != nil {
				t.Fatal(err)
			}
			output, err := NewContract[string]("test.identity-output", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := host.Register(input, output); err != nil {
				t.Fatal(err)
			}
			dependency, err := Define(Definition{Revision: value.Hash([]byte("identity-dependency")), Units: []Unit{{
				Declaration: UnitDeclaration{ID: "provider", Profile: NativeGo, Exports: []ExportDeclaration{{Name: "value", Contract: input.Ref()}}},
				Setup: func(_ context.Context, setup *Setup) ([]ExportValue, error) {
					return []ExportValue{Provide("value", input, string(setup.Configuration()))}, nil
				},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			rootPlugin, err := Define(Definition{Revision: value.Hash([]byte("identity-root")), Units: []Unit{{
				Declaration: UnitDeclaration{ID: "consumer", Profile: NativeGo, Imports: []ImportDeclaration{{Slot: "input", Contract: input.Ref()}}, Exports: []ExportDeclaration{{Name: "value", Contract: output.Ref()}}},
				Setup: func(_ context.Context, setup *Setup) ([]ExportValue, error) {
					provider, err := Import(setup, "input", input)
					if err != nil {
						return nil, err
					}
					return []ExportValue{Provide("value", output, string(setup.Configuration())+"/"+provider)}, nil
				},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, config := range []InstallationConfig{{ID: "dependency", Plugin: dependency, Configuration: []byte("before"), Enabled: true}, {ID: "root", Plugin: rootPlugin, Configuration: []byte("before"), Enabled: true}} {
				if _, err := host.Install(ctx, config); err != nil {
					t.Fatal(err)
				}
			}
			root := ExportRef{InstallationID: "root", Release: rootPlugin.Revision(), UnitID: "consumer", Export: "value", Contract: output.Ref()}
			planRoot := func() (BindingPlan, error) {
				if scenario.unitRoot {
					return host.PlanUnit(ctx, "", root.Unit(), nil)
				}
				return host.Plan(ctx, root)
			}
			plan, err := planRoot()
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := host.activate(ctx, "", ScopeGrant{}, host.plannedBinding(plan.bindingPlan))
			if err != nil {
				t.Fatal(err)
			}
			accepted = prepared.binding.Clone()
			original := accepted.Clone()
			plugin := rootPlugin
			if scenario.changed == "dependency" {
				plugin = dependency
			}
			// The two existing acquisition stages provide a deterministic pause
			// without adding a production hook or relying on scheduler timing.
			if !scenario.sameBinding {
				if err := host.Replace(ctx, scenario.changed, plugin, []byte("after")); err != nil {
					t.Fatal(err)
				}
			}
			for _, installation := range []InstallationID{"root", scenario.changed} {
				if err := host.Retire(ctx, installation); err != nil {
					t.Fatal(err)
				}
				if err := host.WaitRetired(ctx, installation); err != nil {
					t.Fatal(err)
				}
			}
			freshPlan, err := planRoot()
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := host.ActivatePlan(ctx, "", freshPlan)
			if err != nil {
				t.Fatal(err)
			}
			if same, err := original.sameIdentity(fresh); err != nil || same != scenario.sameBinding {
				t.Fatalf("replacement setup identity differs from its configuration: same=%t error=%v", same, err)
			}
			config := UseConfig{Owner: owner, Scope: scope}
			var use *Use
			use, err = host.acquirePrepared(ctx, prepared, config, scenario.recordID)
			if !errors.Is(err, ErrStaleBinding) || use != nil {
				if use != nil {
					_, _ = use.Close(ctx)
				}
				t.Fatalf("old binding acquired another setup: use=%v, err=%v", use != nil, err)
			}
			accepted = fresh
			if scenario.recordID != "" {
				use, err = host.AcquireRecord(ctx, scenario.recordID, config)
			} else {
				use, err = host.Acquire(ctx, freshPlan, config)
			}
			if err != nil {
				t.Fatalf("current setup was refused: %v", err)
			}
			if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
				t.Fatalf("current use close: %+v, %v", outcome, err)
			}
		})
	}
}
