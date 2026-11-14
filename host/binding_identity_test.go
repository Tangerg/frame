package host_test

import (
	"context"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestCanonicalBindingRetainsSharedProviderIdentity(t *testing.T) {
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx := testContext(t)
	var accepted framehost.BindingSnapshot
	host := testHost(t, framehost.Config{ReadAcceptedBinding: func(context.Context, string, framehost.ScopeGrant) (framehost.BindingSnapshot, error) {
		return accepted.Clone(), nil
	}})
	a, err := framehost.NewContract[string]("a", "1")
	must(err)
	z, err := framehost.NewContract[string]("z", "1")
	must(err)
	middle, err := framehost.NewContract[string]("middle", "1")
	must(err)
	point, err := framehost.NewPoint[string, string]("points", "1")
	must(err)
	must(host.Register(a, z, middle, point))
	install := func(id string, unit framehost.Unit) framehost.InstallationRecord {
		p, e := framehost.Define(framehost.Definition{Revision: value.Hash([]byte(id)), Units: []framehost.Unit{unit}})
		must(e)
		r, e := host.Install(ctx, framehost.InstallationConfig{ID: framehost.InstallationID(id), Plugin: p, Enabled: true})
		must(e)
		return r
	}
	source := func(r framehost.InstallationRecord, name string, c framehost.ContractRef) framehost.ExportRef {
		return framehost.ExportRef{InstallationID: r.ID, Release: r.Release, UnitID: "main", Export: name, Contract: c}
	}
	ar := install("a", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "a", Contract: a.Ref()}}}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
		return []framehost.ExportValue{framehost.Provide("a", a, "a")}, nil
	}})
	zr := install("z", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "z", Contract: z.Ref()}}}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
		return []framehost.ExportValue{framehost.Provide("z", z, "z")}, nil
	}})
	mr := install("middle", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "z", Contract: z.Ref()}, {Slot: "a", Contract: a.Ref()}}, Fixed: []framehost.SlotProviders{{Slot: "z", Providers: []framehost.ExportRef{source(zr, "z", z.Ref())}}, {Slot: "a", Providers: []framehost.ExportRef{source(ar, "a", a.Ref())}}}, Exports: []framehost.ExportDeclaration{{Name: "middle", Contract: middle.Ref()}}}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
		return []framehost.ExportValue{framehost.Provide("middle", middle, "middle")}, nil
	}})
	makePoint := func(id string) framehost.InstallationRecord {
		return install(id, framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "middle", Contract: middle.Ref()}}, Fixed: []framehost.SlotProviders{{Slot: "middle", Providers: []framehost.ExportRef{source(mr, "middle", middle.Ref())}}}, Exports: []framehost.ExportDeclaration{{Name: "point", Contract: point.Ref()}}}, Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("point", point, id, id)}, nil
		}})
	}
	first := makePoint("first")
	second := makePoint("second")
	_, err = host.Activate(ctx, source(first, "point", point.Ref()))
	must(err)
	plan, err := host.Plan(ctx, source(second, "point", point.Ref()))
	must(err)
	original, err := plan.Snapshot()
	must(err)
	encoded, err := original.CanonicalJSON()
	must(err)
	accepted, err = framehost.ParseBinding(encoded)
	must(err)
	id1, err := original.Identity()
	must(err)
	id2, err := accepted.Identity()
	must(err)
	if id1 != id2 {
		t.Fatal("canonical serialization changed binding identity")
	}
	owner, scope := ownerAndScope(t, host)
	use, err := host.AcquireRecord(ctx, "saved", framehost.UseConfig{Owner: owner, Scope: scope})
	must(err)
	snapshot, err := host.Snapshot(ctx, point.Ref())
	must(err)
	candidates, err := framehost.Candidates(snapshot, point)
	must(err)
	selected, err := framehost.Select(snapshot, point, "all", candidates...)
	must(err)
	_, err = framehost.Capture(snapshot, selected.Ref())
	if len(candidates) != 2 || err != nil {
		t.Fatalf("canonical shared binding was rejected: members=%d error=%v", len(candidates), err)
	}
	_, err = use.Close(ctx)
	must(err)
}

func TestBindingIdentitySurvivesPublicJSONRoundTrip(t *testing.T) {
	host := testHost(t, framehost.Config{})
	service, err := framehost.NewContract[string]("binding.text", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "text", Contract: service.Ref()}}},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Provide("text", service, "value")}, nil
		},
	})
	plan, err := host.Plan(t.Context(), source("provider", plugin, "main", "text", service.Ref()))
	if err != nil {
		t.Fatal(err)
	}
	original, err := plan.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	before, err := original.Identity()
	if err != nil {
		t.Fatal(err)
	}
	for _, unitRoots := range [][]framehost.UnitRef{nil, {}} {
		original.UnitRoots = unitRoots
		encoded, err := value.EncodeJSON(original)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := framehost.ParseBinding(encoded.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		after, err := decoded.Identity()
		if err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Fatalf("public serialization changed binding identity: before=%s, after=%s", before, after)
		}
	}
}
