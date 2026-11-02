package frame_test

import (
	"context"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type realmValueDriver struct {
	contract frame.Contract[string]
	bound    bool
}

func (realmValueDriver) Profile() frame.Profile { return "test.realm-value" }
func (d realmValueDriver) RealmBound() bool     { return d.bound }
func (d realmValueDriver) Prepare(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	return []frame.ExportValue{frame.Provide("value", d.contract, string(setup.Realm()))}, nil
}

type realmUseProtection struct{}

func (realmUseProtection) ApproveUse(context.Context, frame.UseApproval) error { return nil }
func (realmUseProtection) ReleaseUse(context.Context, string, frame.CloseOutcome) error {
	return nil
}

func TestExactRootUsesItsDriverRealmWhenOtherRealmsAreReady(t *testing.T) {
	for _, mode := range []frame.ContractMode{frame.NativeMode, frame.PortableMode} {
		for _, bound := range []bool{true, false} {
			name := "shared execution"
			if bound {
				name = "realm-bound execution"
			}
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				ctx := testContext(t)
				descriptor := []byte(`{"protocol":"realm-value-test"}`)
				contract, err := frame.NewContract[string]("test.realm-value", "1")
				if mode == frame.PortableMode {
					contract, err = frame.BindContract[string](frame.ContractRef{Name: "test.realm-value", Version: "1", Kind: frame.ServiceKind, Mode: mode, Digest: value.Hash(descriptor)}, descriptor)
				}
				if err != nil {
					t.Fatal(err)
				}
				driver := realmValueDriver{contract: contract, bound: bound}
				var accepted frame.BindingSnapshot
				host := testHost(t, frame.HostConfig{Drivers: []frame.Driver{driver}, ExecutionProtection: realmUseProtection{}, ReadAcceptedBinding: func(context.Context, string, frame.ScopeGrant) (frame.BindingSnapshot, error) {
					return accepted.Clone(), nil
				}})
				if err := host.Register(contract); err != nil {
					t.Fatal(err)
				}
				plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("realm-value-test")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "value", Profile: driver.Profile(), Exports: []frame.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := host.Install(ctx, frame.InstallationConfig{ID: "provider", Plugin: plugin, Enabled: true}); err != nil {
					t.Fatal(err)
				}
				root := source("provider", plugin, "value", "value", contract.Ref())
				owner, scope := ownerAndScope(t, host)
				for _, realm := range []frame.RealmID{"realm-a", "realm-b"} {
					if _, err := host.ActivateIn(ctx, realm, root); err != nil {
						t.Fatal(err)
					}
				}
				for _, realm := range []frame.RealmID{"realm-a", "realm-b"} {
					plan, err := host.PlanWith(ctx, realm, nil, root)
					if err != nil {
						t.Fatal(err)
					}
					accepted, err = plan.Snapshot()
					if err != nil {
						t.Fatal(err)
					}
					for _, restore := range []bool{false, true} {
						var use *frame.Use
						config := frame.UseConfig{Owner: owner, Scope: scope, Realm: realm}
						if restore {
							use, err = host.AcquireRecord(ctx, "accepted-record", config)
						} else {
							use, err = host.Acquire(ctx, plan, config)
						}
						if err != nil {
							t.Fatalf("acquire %s: %v", realm, err)
						}
						if err := frame.BorrowExport(ctx, use, root, func(_ context.Context, actual string) error {
							var expected string
							if bound {
								expected = string(realm)
							}
							if actual != expected {
								t.Errorf("borrowed another execution's root: got %q, want %q", actual, expected)
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
							t.Fatalf("close: %+v, %v", outcome, err)
						}
					}
				}
			})
		}
	}
}
