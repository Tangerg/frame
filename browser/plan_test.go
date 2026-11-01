package browser_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/value"
)

func TestBridgeFreshPlanPreservesPreparationPremises(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	optional, err := frame.NewContract[browser.Object]("test.optional", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.host.Register(optional); err != nil {
		t.Fatal(err)
	}
	consumer, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("optional-browser")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{
		ID: "view", Profile: frame.BrowserJS,
		Exports: []frame.ExportDeclaration{{Name: "preview", Contract: e.first.Ref()}},
		Imports: []frame.ImportDeclaration{{Slot: "optional", Contract: optional.Ref(), Optional: true}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.host.Install(e.ctx, frame.InstallationConfig{ID: "consumer", Plugin: consumer, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.credentials, err = e.bridge.OpenRealm(e.ctx, "prepared-realm", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	root := frame.ExportRef{InstallationID: "consumer", Release: consumer.Revision(), UnitID: "view", Export: "preview", Contract: e.first.Ref()}
	plan, err := e.host.PlanWith(e.ctx, browser.RealmID(e.credentials.Realm), nil, root)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("late-provider")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "provider", Profile: frame.BrowserJS, Exports: []frame.ExportDeclaration{{Name: "optional", Contract: optional.Ref()}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.host.Install(e.ctx, frame.InstallationConfig{ID: "late", Plugin: provider, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.AcquirePlan(e.ctx, e.credentials.Token, "prepared-use", plan, ""); !errors.Is(err, frame.ErrStaleBinding) {
		t.Fatalf("browser adapter converted a fresh plan into a restored binding: %v", err)
	}
	attempts, err := e.store.Attempts(e.ctx)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("stale browser plan created an execution: %+v, %v", attempts, err)
	}
}

func TestBridgeAcquiresDeclarationPlanInItsOriginalRealm(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	execution := e.start("component")
	root := frame.ExportRef{InstallationID: "component", Release: e.plugin.Revision(), UnitID: "view", Export: "preview", Contract: e.first.Ref()}
	plan, err := e.host.PlanWith(e.ctx, browser.RealmID(e.credentials.Realm), nil, root)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := e.bridge.AcquirePlan(e.ctx, e.credentials.Token, "component-use", plan, "")
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := e.bridge.AcquirePlan(e.ctx, e.credentials.Token, "component-use", plan, "")
	if err != nil || repeated.ID != grant.ID || grant.Execution != execution.Execution || len(grant.Objects) != 1 {
		t.Fatalf("original plan lost its execution or repeated its grant: %+v, %+v, %v", grant, repeated, err)
	}
	if _, err := e.bridge.Prove(e.ctx, e.credentials.Token, useProof(execution, grant)); err != nil {
		t.Fatal(err)
	}
}
