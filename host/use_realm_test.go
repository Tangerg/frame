package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type realmValueDriver struct {
	contract framehost.Contract[string]
	bound    bool
}

func (realmValueDriver) Profile() framehost.Profile { return "test.realm-value" }
func (d realmValueDriver) RealmBound() bool         { return d.bound }
func (d realmValueDriver) Prepare(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
	return []framehost.ExportValue{framehost.Provide("value", d.contract, string(setup.Realm()))}, nil
}

type realmUseProtection struct{}

func (realmUseProtection) ApproveUse(context.Context, framehost.UseApproval) (framehost.CommitState, error) {
	return framehost.Committed, nil
}
func (realmUseProtection) SettleUse(context.Context, string, frame.ReleaseProof) error {
	return nil
}

type countedRealmValueDriver struct {
	realmValueDriver
	preparations atomic.Int32
}

func (d *countedRealmValueDriver) Prepare(ctx context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
	d.preparations.Add(1)
	return d.realmValueDriver.Prepare(ctx, setup)
}

func TestPendingConsumerRetainsItsOriginalProviderInstances(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		contract, err := framehost.NewContract[string]("preparation.original-realm", "1")
		if err != nil {
			t.Fatal(err)
		}
		driver := &countedRealmValueDriver{realmValueDriver: realmValueDriver{contract: contract, bound: true}}
		host := testHost(t, framehost.Config{Drivers: []framehost.Driver{driver}})
		if err := host.Register(contract); err != nil {
			t.Fatal(err)
		}
		provider := installNative(t, host, "provider", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: driver.Profile(), Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}})
		entered, release := make(chan struct{}), make(chan struct{})
		consumer := installNative(t, host, "consumer", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "value", Contract: contract.Ref()}}, Fixed: []framehost.SlotProviders{{Slot: "value", Providers: []framehost.ExportRef{source("provider", provider, "main", "value", contract.Ref())}}}, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				close(entered)
				<-release
				value, err := framehost.Import(setup, "value", contract)
				return []framehost.ExportValue{framehost.Provide("value", contract, value)}, err
			},
		})
		root := source("consumer", consumer, "main", "value", contract.Ref())
		first, second := make(chan error, 1), make(chan error, 1)
		go func() { _, err := host.ActivateIn(t.Context(), "realm-a", root); first <- err }()
		<-entered
		go func() { _, err := host.ActivateIn(t.Context(), "realm-b", root); second <- err }()
		synctest.Wait()
		var refused error
		select {
		case refused = <-second:
		default:
			close(release)
			<-first
			<-second
			t.Fatal("another provider graph waited for the original consumer's preparation")
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatalf("refused graph invalidated original preparation: %v", err)
		}
		if !errors.Is(refused, framehost.ErrInUse) || driver.preparations.Load() != 1 {
			t.Fatalf("pending consumer acquired competing provider instances: preparations=%d error=%v", driver.preparations.Load(), refused)
		}
		if _, err := host.ActivateIn(t.Context(), "realm-a", root); err != nil || driver.preparations.Load() != 1 {
			t.Fatalf("original preparation was replaced: preparations=%d error=%v", driver.preparations.Load(), err)
		}
	})
}

func TestBindingPreparationRetainsExactProviderInstances(t *testing.T) {
	for _, transitive := range []bool{false, true} {
		name := "direct"
		if transitive {
			name = "transitive"
		}
		for _, entry := range []string{"activation", "declaration", "record"} {
			t.Run(name+"/"+entry, func(t *testing.T) {
				ctx := testContext(t)
				contract, err := framehost.NewContract[string]("binding.original-realm", "1")
				if err != nil {
					t.Fatal(err)
				}
				driver := &countedRealmValueDriver{realmValueDriver: realmValueDriver{contract: contract, bound: true}}
				var accepted framehost.BindingSnapshot
				host := testHost(t, framehost.Config{Drivers: []framehost.Driver{driver}, Authority: protectionAuthority(realmUseProtection{}), ReadAcceptedBinding: func(context.Context, string, framehost.ScopeGrant) (framehost.BindingSnapshot, error) {
					return accepted.Clone(), nil
				}})
				if err := host.Register(contract); err != nil {
					t.Fatal(err)
				}
				provider := installNative(t, host, "provider", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: driver.Profile(), Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}})
				forward := func(id string, dependency framehost.ExportRef) *framehost.Plugin {
					return installNative(t, host, id, framehost.Unit{
						Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "value", Contract: contract.Ref()}}, Fixed: []framehost.SlotProviders{{Slot: "value", Providers: []framehost.ExportRef{dependency}}}, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
						Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
							value, err := framehost.Import(setup, "value", contract)
							return []framehost.ExportValue{framehost.Provide("value", contract, value)}, err
						},
					})
				}
				dependency := source("provider", provider, "main", "value", contract.Ref())
				if transitive {
					middle := forward("middle", dependency)
					dependency = source("middle", middle, "main", "value", contract.Ref())
				}
				consumer := forward("consumer", dependency)
				root := source("consumer", consumer, "main", "value", contract.Ref())
				accepted, err = host.ActivateIn(ctx, "realm-a", root)
				if err != nil {
					t.Fatal(err)
				}
				owner, scope := ownerAndScope(t, host)
				original, err := acquireExports(host, ctx, accepted, framehost.UseConfig{Owner: owner, Scope: scope, Realm: "realm-a"})
				if err != nil {
					t.Fatal(err)
				}
				defer original.Close(context.Background())
				originals := make(map[string]bool)
				for _, observation := range host.Observations() {
					originals[observation.ID] = true
				}
				config := framehost.UseConfig{Owner: owner, Scope: scope, Realm: "realm-b"}
				switch entry {
				case "activation":
					_, err = host.ActivateIn(ctx, config.Realm, root)
				case "declaration":
					var use framehost.Use
					use, err = host.AcquireUnit(ctx, root.Unit(), config)
					if use.Valid() {
						_, _ = use.Close(ctx)
					}
				case "record":
					var use framehost.Use
					use, err = host.AcquireRecord(ctx, "original-record", config)
					if use.Valid() {
						_, _ = use.Close(ctx)
					}
				}
				if !errors.Is(err, framehost.ErrInUse) || driver.preparations.Load() != 1 {
					t.Fatalf("another physical provider graph reused the original consumer: preparations=%d error=%v", driver.preparations.Load(), err)
				}
				for _, observation := range host.Observations() {
					if !originals[observation.ID] {
						if err := host.WaitInstanceRetired(ctx, observation.ID); err != nil {
							t.Fatalf("refused graph stranded an unstarted instance: %v", err)
						}
					}
				}
				if err := framehost.BorrowExport(ctx, original, root, func(_ context.Context, value string) error {
					if value != "realm-a" {
						t.Errorf("refused graph rewrote the original provider: %q", value)
					}
					return nil
				}); err != nil {
					t.Fatalf("refused graph invalidated an accepted borrow: %v", err)
				}
				if err := host.Admission().RecoveryError(); err != nil {
					t.Fatalf("binding refusal fenced unrelated work: %v", err)
				}
				if err := host.Retire(ctx, "consumer"); err != nil {
					t.Fatal(err)
				}
				if outcome, err := original.Close(ctx); !outcome.Released || err != nil {
					t.Fatalf("original borrow did not retire: %+v, %v", outcome, err)
				}
				if err := host.WaitRetired(ctx, "consumer"); err != nil {
					t.Fatal(err)
				}
				if transitive {
					if err := host.Retire(ctx, "middle"); err != nil {
						t.Fatal(err)
					}
					if err := host.WaitRetired(ctx, "middle"); err != nil {
						t.Fatal(err)
					}
				}
				next, err := host.AcquireRecord(ctx, "original-record", config)
				if err != nil {
					t.Fatalf("original retirement did not admit a fresh realization: %v", err)
				}
				defer next.Close(context.Background())
				if err := framehost.BorrowExport(ctx, next, root, func(_ context.Context, value string) error {
					if value != "realm-b" {
						t.Errorf("fresh preparation adopted an old realm: %q", value)
					}
					return nil
				}); err != nil || driver.preparations.Load() != 2 {
					t.Fatalf("fresh graph did not own its original provider: preparations=%d error=%v", driver.preparations.Load(), err)
				}
			})
		}
	}
}

func TestExactRootUsesItsDriverRealmWhenOtherRealmsAreReady(t *testing.T) {
	for _, mode := range []framehost.ContractMode{framehost.NativeMode, framehost.PortableMode} {
		for _, bound := range []bool{true, false} {
			name := "shared execution"
			if bound {
				name = "realm-bound execution"
			}
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				ctx := testContext(t)
				descriptor := []byte(`{"protocol":"realm-value-test"}`)
				contract, err := framehost.NewContract[string]("test.realm-value", "1")
				if mode == framehost.PortableMode {
					contract, err = framehost.BindContract[string](framehost.ContractRef{Name: "test.realm-value", Version: "1", Kind: framehost.ServiceKind, Mode: mode, Digest: value.Hash(descriptor)}, descriptor)
				}
				if err != nil {
					t.Fatal(err)
				}
				driver := realmValueDriver{contract: contract, bound: bound}
				var accepted framehost.BindingSnapshot
				host := testHost(t, framehost.Config{Drivers: []framehost.Driver{driver}, Authority: protectionAuthority(realmUseProtection{}), ReadAcceptedBinding: func(context.Context, string, framehost.ScopeGrant) (framehost.BindingSnapshot, error) {
					return accepted.Clone(), nil
				}})
				if err := host.Register(contract); err != nil {
					t.Fatal(err)
				}
				plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("realm-value-test")), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{ID: "value", Profile: driver.Profile(), Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := host.Install(ctx, framehost.InstallationConfig{ID: "provider", Plugin: plugin, Enabled: true}); err != nil {
					t.Fatal(err)
				}
				root := source("provider", plugin, "value", "value", contract.Ref())
				owner, scope := ownerAndScope(t, host)
				for _, realm := range []framehost.RealmID{"realm-a", "realm-b"} {
					if _, err := host.ActivateIn(ctx, realm, root); err != nil {
						t.Fatal(err)
					}
				}
				for _, realm := range []framehost.RealmID{"realm-a", "realm-b"} {
					plan, err := host.PlanWith(ctx, realm, nil, root)
					if err != nil {
						t.Fatal(err)
					}
					accepted, err = plan.Snapshot()
					if err != nil {
						t.Fatal(err)
					}
					for _, restore := range []bool{false, true} {
						var use framehost.Use
						config := framehost.UseConfig{Owner: owner, Scope: scope, Realm: realm}
						if restore {
							use, err = host.AcquireRecord(ctx, "accepted-record", config)
						} else {
							use, err = host.Acquire(ctx, plan, config)
						}
						if err != nil {
							t.Fatalf("acquire %s: %v", realm, err)
						}
						if err := framehost.BorrowExport(ctx, use, root, func(_ context.Context, actual string) error {
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

func TestAcquireUnitInheritsBeforePlanning(t *testing.T) {
	for _, bound := range []bool{false, true} {
		name := "shared execution"
		if bound {
			name = "realm-bound execution"
		}
		t.Run(name, func(t *testing.T) {
			contract, err := framehost.NewContract[string]("example.inherited-realm", "1")
			if err != nil {
				t.Fatal(err)
			}
			driver := realmValueDriver{contract: contract, bound: bound}
			host := testHost(t, framehost.Config{Drivers: []framehost.Driver{driver}, Authority: protectionAuthority(realmUseProtection{})})
			if err := host.Register(contract); err != nil {
				t.Fatal(err)
			}
			plugin := installNative(t, host, "provider", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: driver.Profile(), Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}})
			root := framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}
			owner, scope := ownerAndScope(t, host)
			parent, err := host.AcquireUnit(t.Context(), root, framehost.UseConfig{Owner: owner, Scope: scope, Realm: "original-realm", ExecutionID: "original-execution"})
			if err != nil {
				t.Fatal(err)
			}
			for _, explicit := range []bool{false, true} {
				config := framehost.UseConfig{Owner: parent.Lifetime(), Parent: parent}
				if explicit {
					config.Realm, config.Scope, config.ExecutionID = "original-realm", scope, "original-execution"
				}
				child, err := host.AcquireUnit(t.Context(), root, config)
				if err != nil {
					t.Fatalf("inherited acquisition, explicit=%t: %v", explicit, err)
				}
				grandchild, err := host.AcquireUnit(t.Context(), root, framehost.UseConfig{Owner: child.Lifetime(), Parent: child})
				if err != nil {
					t.Fatalf("transitive inherited acquisition: %v", err)
				}
				if child.Realm() != parent.Realm() || grandchild.Realm() != parent.Realm() {
					t.Fatal("a descendant changed its original realm")
				}
				if binding := grandchild.Binding(); len(binding.UnitRoots) != 1 || binding.UnitRoots[0] != root || len(host.Observations()) != 1 {
					t.Fatalf("descendant prepared another unit or execution: %+v", binding)
				}
				if _, err := child.Close(testContext(t)); err != nil {
					t.Fatal(err)
				}
			}
			otherScope, err := host.Admission().OpenScope(t.Context(), "other")
			if err != nil {
				t.Fatal(err)
			}
			for _, config := range []framehost.UseConfig{
				{Owner: parent.Lifetime(), Parent: parent, Realm: "other-realm"},
				{Owner: parent.Lifetime(), Parent: parent, Scope: otherScope},
				{Owner: parent.Lifetime(), Parent: parent, ExecutionID: "other-execution"},
			} {
				if use, err := host.AcquireUnit(t.Context(), root, config); use.Valid() || !errors.Is(err, frame.ErrInvalid) {
					t.Fatalf("conflicting child escaped its parent: %v, %v", use, err)
				}
			}
			if _, err := parent.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if use, err := host.AcquireUnit(t.Context(), root, framehost.UseConfig{Owner: parent.Lifetime(), Parent: parent}); use.Valid() || !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("closed parent admitted another descendant: %v, %v", use, err)
			}
			if use, err := host.AcquireUnit(t.Context(), root, framehost.UseConfig{}); use.Valid() || !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("missing owner was not refused at the boundary: %v, %v", use, err)
			}
		})
	}
}
