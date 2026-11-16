package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestCapturedBindingReconstructionUsesOriginalInstanceRealms(t *testing.T) {
	for _, graph := range []struct {
		name      string
		consumers []string
	}{
		{name: "direct provider"},
		{name: "shared consumer", consumers: []string{"consumer"}},
		{name: "transitive provider", consumers: []string{"middle", "consumer"}},
	} {
		for _, realmBound := range []bool{false, true} {
			profile := "shared execution"
			if realmBound {
				profile = "realm execution"
			}
			for _, realm := range []framehost.RealmID{"original-realm", "another-realm"} {
				t.Run(graph.name+"/"+profile+"/"+string(realm), func(t *testing.T) {
					ctx := testContext(t)
					descriptor := []byte(`{"protocol":"original-realm-reconstruction"}`)
					contract, err := framehost.BindContract[string](framehost.ContractRef{Name: "binding.reconstruction", Version: "1", Kind: framehost.ServiceKind, Mode: framehost.PortableMode, Digest: value.Hash(descriptor)}, descriptor)
					if err != nil {
						t.Fatal(err)
					}
					driver := &countedRealmValueDriver{realmValueDriver: realmValueDriver{contract: contract, bound: realmBound}}
					var accepted framehost.BindingSnapshot
					host := testHost(t, framehost.Config{Drivers: []framehost.Driver{driver}, Authority: protectionAuthority(realmUseProtection{}), ReadAcceptedBinding: func(context.Context, string, framehost.ScopeGrant) (framehost.BindingSnapshot, error) {
						return accepted.Clone(), nil
					}})
					if err := host.Register(contract); err != nil {
						t.Fatal(err)
					}
					provider := installNative(t, host, "provider", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: driver.Profile(), Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}})
					root := source("provider", provider, "main", "value", contract.Ref())
					for _, id := range graph.consumers {
						consumer := installNative(t, host, id, framehost.Unit{
							Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "value", Contract: contract.Ref()}}, Fixed: []framehost.SlotProviders{{Slot: "value", Providers: []framehost.ExportRef{root}}}, Exports: []framehost.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}},
							Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
								text, err := framehost.Import(setup, "value", contract)
								return []framehost.ExportValue{framehost.Provide("value", contract, text)}, err
							},
						})
						root = source(id, consumer, "main", "value", contract.Ref())
					}
					if _, err := host.ActivateIn(ctx, "original-realm", root); err != nil {
						t.Fatal(err)
					}
					snapshot, err := host.SnapshotIn(ctx, realm, contract.Ref())
					if err != nil {
						t.Fatal(err)
					}
					candidates, err := snapshot.Candidates(contract.Ref())
					if err != nil {
						t.Fatal(err)
					}
					var original framehost.CandidateRef
					for _, candidate := range candidates {
						if candidate.Source() == root {
							original = candidate
						}
					}
					selection, err := framehost.SelectRefs(snapshot, contract.Ref(), "original", original)
					if err != nil {
						t.Fatal(err)
					}
					plan, err := framehost.Capture(snapshot, selection)
					if err != nil {
						t.Fatal(err)
					}
					owner, scope := ownerAndScope(t, host)
					config := framehost.UseConfig{Owner: owner, Scope: scope, Realm: realm}
					use, err := host.Acquire(ctx, plan, config)
					if err != nil {
						t.Fatalf("live portable binding lost its original instance: %v", err)
					}
					defer use.Close(context.Background())
					expected := ""
					if realmBound {
						expected = "original-realm"
					}
					if err := framehost.BorrowExport(ctx, use, root, func(_ context.Context, actual string) error {
						if actual != expected {
							t.Errorf("portable borrow retargeted the captured instance: got=%q want=%q", actual, expected)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					accepted, err = plan.Snapshot()
					foreign := realmBound && realm != "original-realm"
					if foreign {
						if !errors.Is(err, frame.ErrInvalid) {
							t.Fatalf("cross-realm instances became reconstructible routing evidence: binding=%+v error=%v", accepted, err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
						t.Fatalf("original Use did not release its holds: outcome=%+v error=%v", outcome, err)
					}
					for _, id := range append([]string{"provider"}, graph.consumers...) {
						if err := host.Retire(ctx, framehost.InstallationID(id)); err != nil {
							t.Fatal(err)
						}
					}
					for _, id := range append([]string{"provider"}, graph.consumers...) {
						if err := host.WaitRetired(ctx, framehost.InstallationID(id)); err != nil {
							t.Fatal(err)
						}
					}
					if foreign {
						if _, err := plan.Snapshot(); !errors.Is(err, frame.ErrInvalid) {
							t.Fatalf("release proof erased original Realm provenance: %v", err)
						}
						return
					}
					restored, err := host.AcquireRecord(ctx, "original-record", config)
					if err != nil {
						t.Fatalf("reconstructible binding could not restore: %v", err)
					}
					defer restored.Close(context.Background())
					if err := framehost.BorrowExport(ctx, restored, root, func(_ context.Context, actual string) error {
						if actual != expected {
							t.Errorf("restored binding selected another execution: got=%q want=%q", actual, expected)
						}
						return nil
					}); err != nil || driver.preparations.Load() != 2 {
						t.Fatalf("restoration changed binding or preparation ownership: preparations=%d error=%v", driver.preparations.Load(), err)
					}
					if use, err := host.Acquire(ctx, plan, config); use.Valid() || !errors.Is(err, framehost.ErrStaleBinding) {
						t.Fatalf("original captured roots adopted successor instances: valid=%t error=%v", use.Valid(), err)
					}
				})
			}
		}
	}
}
