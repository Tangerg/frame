package frame

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tangerg/frame/value"
)

func TestReadyBindingCannotAcquireAnotherSetupIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		unitRoot bool
		changed  InstallationID
		recordID string
	}{
		{"export/root/plan", false, "root", ""},
		{"export/root/record", false, "root", "original-binding"},
		{"export/dependency/plan", false, "dependency", ""},
		{"export/dependency/record", false, "dependency", "original-binding"},
		{"unit/root/plan", true, "root", ""},
		{"unit/root/record", true, "root", "original-binding"},
		{"unit/dependency/plan", true, "dependency", ""},
		{"unit/dependency/record", true, "dependency", "original-binding"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var accepted BindingSnapshot
			host, err := NewHost(context.Background(), HostConfig{ReadAcceptedBinding: func(context.Context, string, ScopeGrant) (BindingSnapshot, error) {
				return accepted.Clone(), nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			owner := NewLifetime(context.Background())
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
			planRoot := func() (*BindingPlan, error) {
				if scenario.unitRoot {
					return host.PlanUnit(ctx, "", root.Unit(), nil)
				}
				return host.Plan(ctx, root)
			}
			plan, err := planRoot()
			if err != nil {
				t.Fatal(err)
			}
			accepted, err = host.ActivatePlan(ctx, "", plan)
			if err != nil {
				t.Fatal(err)
			}
			original := accepted.Clone()
			plugin := rootPlugin
			if scenario.changed == "dependency" {
				plugin = dependency
			}
			// The two existing acquisition stages provide a deterministic pause
			// without adding a production hook or relying on scheduler timing.
			if err := host.Replace(ctx, scenario.changed, plugin, []byte("after")); err != nil {
				t.Fatal(err)
			}
			for _, installation := range []InstallationID{"root", scenario.changed} {
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
			if same, err := original.sameIdentity(fresh); err != nil || same {
				t.Fatalf("replacement did not change the setup identity: %v", err)
			}
			config := UseConfig{Owner: owner, Scope: scope}
			var use *Use
			plan, err = host.readyPlan(original, config.Realm)
			if err == nil {
				use, err = host.acquirePrepared(ctx, plan, config, scenario.recordID)
			}
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
