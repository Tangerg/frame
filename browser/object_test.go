package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/value"
)

func TestNativeAndBrowserShareContractWithRealmLocalValues(t *testing.T) {
	for _, order := range []struct {
		name            string
		descriptorFirst bool
	}{{"typed first", false}, {"descriptor first", true}} {
		t.Run(order.name, func(t *testing.T) {
			e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
			point, err := frame.NewPoint[testMetadata, string]("test.shared-realm-contract", "1")
			if err != nil {
				t.Fatal(err)
			}
			registerDescriptor := func() {
				t.Helper()
				if err := e.host.RegisterDescriptor(point.Ref(), point.Descriptor()); err != nil {
					t.Fatal(err)
				}
			}
			if order.descriptorFirst {
				registerDescriptor()
			}
			if err := e.host.Register(point); err != nil {
				t.Fatal(err)
			}
			if !order.descriptorFirst {
				registerDescriptor()
			}
			native, err := frame.Define(frame.Definition{Revision: value.Hash([]byte("native-shared-contract")), Units: []frame.Unit{{
				Declaration: frame.UnitDeclaration{ID: "view", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "shared", Contract: point.Ref()}}},
				Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
					return []frame.ExportValue{frame.Contribute("shared", point, testMetadata{Label: "native"}, "native value")}, nil
				},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.host.Install(e.ctx, frame.InstallationConfig{ID: "native", Plugin: native, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			nativeSource := frame.ExportRef{InstallationID: "native", Release: native.Revision(), UnitID: "view", Export: "shared", Contract: point.Ref()}
			plan, err := e.host.Plan(e.ctx, nativeSource)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := e.host.Admission().OpenScope(e.ctx, "native-contract-use")
			if err != nil {
				t.Fatal(err)
			}
			use, err := e.host.Acquire(e.ctx, plan, frame.UseConfig{Owner: frame.NewLifetime(e.ctx), Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			if err := frame.BorrowExport(e.ctx, use, nativeSource, func(_ context.Context, actual string) error {
				if actual != "native value" {
					t.Errorf("native value was replaced: %q", actual)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := use.Close(e.ctx); err != nil {
				t.Fatal(err)
			}
			e.plugin, err = frame.Define(frame.Definition{Revision: value.Hash([]byte("browser-shared-contract")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{
				ID: "view", Profile: frame.BrowserJS,
				Exports: []frame.ExportDeclaration{{Name: "preview", Contract: e.first.Ref()}, {Name: "theme", Contract: e.second.Ref()}, {Name: "shared", Contract: point.Ref()}},
			}}}})
			if err != nil {
				t.Fatal(err)
			}
			browserPlan := e.start("browser")
			reference := browser.ObjectRef{Export: browser.LocalExport{InstanceID: browserPlan.Execution.ID, Name: "additional", Contract: point.Ref()}, ObjectID: "additional-object"}
			published, err := e.bridge.Publish(e.ctx, e.credentials.Token, browser.PublicationRequest{
				OperationID: "additional-publication", Authority: browserPlan.Authority, Realm: browserPlan.Realm, Execution: browserPlan.Execution,
				Object: reference, MetadataJSON: `{"label":"browser"}`,
			})
			if err != nil || published.Decision != browser.Accepted {
				t.Fatalf("browser-local dynamic value was refused: %+v %v", published, err)
			}
		})
	}
}

func TestBrowserMetadataSurvivesGoTypeRegistrationAfterPublication(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	point, err := frame.NewPoint[testMetadata, string]("test.late-go-projection", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.host.RegisterDescriptor(point.Ref(), point.Descriptor()); err != nil {
		t.Fatal(err)
	}
	e.plugin, err = frame.Define(frame.Definition{Revision: value.Hash([]byte("browser-late-projection")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{
		ID: "view", Profile: frame.BrowserJS,
		Exports: []frame.ExportDeclaration{{Name: "preview", Contract: e.first.Ref()}, {Name: "theme", Contract: e.second.Ref()}, {Name: "shared", Contract: point.Ref()}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	plan := e.start("browser")
	publish := func(name string) {
		t.Helper()
		result, err := e.bridge.Publish(e.ctx, e.credentials.Token, browser.PublicationRequest{
			OperationID: name, Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution,
			Object:       browser.ObjectRef{Export: browser.LocalExport{InstanceID: plan.Execution.ID, Name: name, Contract: point.Ref()}, ObjectID: name + "-object"},
			MetadataJSON: `{"label":"` + name + `"}`,
		})
		if err != nil || result.Decision != browser.Accepted {
			t.Fatalf("publish %s: %+v %v", name, result, err)
		}
	}
	publish("before")
	original, err := e.host.SnapshotIn(e.ctx, browser.RealmID(plan.Realm), point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.host.Register(point); err != nil {
		t.Fatal(err)
	}
	assertMetadata := func(snapshot *frame.Snapshot, labels map[string]string) {
		t.Helper()
		candidates, err := frame.Candidates(snapshot, point)
		if err != nil || len(candidates) != len(labels) {
			t.Fatalf("typed projection cannot read the original publication: %+v %v", candidates, err)
		}
		for _, candidate := range candidates {
			metadata, err := candidate.Metadata()
			label, present := labels[candidate.Source().Export]
			if err != nil || !present || metadata.Label != label {
				t.Fatalf("metadata changed with Go registration: %+v %v", metadata, err)
			}
		}
	}
	assertMetadata(original, map[string]string{"shared": "browser", "before": "before"})
	publish("after")
	current, err := e.host.SnapshotIn(e.ctx, browser.RealmID(plan.Realm), point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	assertMetadata(current, map[string]string{"shared": "browser", "before": "before", "after": "after"})
	assertMetadata(original, map[string]string{"shared": "browser", "before": "before"})
}

func TestBrowserPublicationRejectsInvalidObjectRepresentations(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	plan := e.start("objects")
	reference := browser.ObjectRef{Export: browser.LocalExport{InstanceID: plan.Execution.ID, Name: "dynamic", Contract: e.first.Ref()}, ObjectID: "dynamic-object"}
	otherInstance, otherName, otherContract, invalidID := reference, reference, reference, reference
	otherInstance.Export.InstanceID = "another-instance"
	otherName.Export.Name = "another-export"
	otherContract.Export.Contract = e.second.Ref()
	invalidID.ObjectID = "invalid object id"
	for _, test := range []struct {
		name  string
		value any
	}{
		{"native value", "not a browser object"},
		{"nil object", (*browser.Object)(nil)},
		{"mutable object pointer", &browser.Object{Reference: reference}},
		{"another instance", browser.Object{Reference: otherInstance}},
		{"another export", browser.Object{Reference: otherName}},
		{"another contract", browser.Object{Reference: otherContract}},
		{"invalid object identity", browser.Object{Reference: invalidID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := e.host.PreparePublication(e.ctx, plan.Execution.ID, frame.ExportValue{
				Name: reference.Export.Name, Contract: e.first.Ref(), Metadata: testMetadata{Label: "dynamic"}, Value: test.value,
			})
			if !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("invalid browser representation was admitted: %v", err)
			}
		})
	}
	if _, err := e.host.PreparePublication(e.ctx, plan.Execution.ID, frame.ExportValue{
		Name: reference.Export.Name, Contract: e.first.Ref(), Metadata: testMetadata{Label: "dynamic"}, Value: browser.Object{Reference: reference},
	}); err != nil {
		t.Fatalf("exact browser representation was refused: %v", err)
	}
}
