package frame_test

import (
	"context"
	"testing"

	"github.com/Tangerg/frame"
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
	var accepted frame.BindingSnapshot
	host := testHost(t, frame.HostConfig{ReadAcceptedBinding: func(context.Context, string, frame.ScopeGrant) (frame.BindingSnapshot, error) {
		return accepted.Clone(), nil
	}})
	a, err := frame.NewContract[string]("a", "1")
	must(err)
	z, err := frame.NewContract[string]("z", "1")
	must(err)
	middle, err := frame.NewContract[string]("middle", "1")
	must(err)
	point, err := frame.NewPoint[string, string]("points", "1")
	must(err)
	must(host.Register(a, z, middle, point))
	install := func(id string, unit frame.Unit) frame.InstallationRecord {
		p, e := frame.Define(frame.Definition{Revision: value.Hash([]byte(id)), Units: []frame.Unit{unit}})
		must(e)
		r, e := host.Install(ctx, frame.InstallationConfig{ID: frame.InstallationID(id), Plugin: p, Enabled: true})
		must(e)
		return r
	}
	source := func(r frame.InstallationRecord, name string, c frame.ContractRef) frame.ExportRef {
		return frame.ExportRef{InstallationID: r.ID, Release: r.Release, UnitID: "main", Export: name, Contract: c}
	}
	ar := install("a", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "a", Contract: a.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return []frame.ExportValue{frame.Provide("a", a, "a")}, nil
	}})
	zr := install("z", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "z", Contract: z.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return []frame.ExportValue{frame.Provide("z", z, "z")}, nil
	}})
	mr := install("middle", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Imports: []frame.ImportDeclaration{{Slot: "z", Contract: z.Ref(), Providers: []frame.ExportRef{source(zr, "z", z.Ref())}}, {Slot: "a", Contract: a.Ref(), Providers: []frame.ExportRef{source(ar, "a", a.Ref())}}}, Exports: []frame.ExportDeclaration{{Name: "middle", Contract: middle.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
		return []frame.ExportValue{frame.Provide("middle", middle, "middle")}, nil
	}})
	makePoint := func(id string) frame.InstallationRecord {
		return install(id, frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Imports: []frame.ImportDeclaration{{Slot: "middle", Contract: middle.Ref(), Providers: []frame.ExportRef{source(mr, "middle", middle.Ref())}}}, Exports: []frame.ExportDeclaration{{Name: "point", Contract: point.Ref()}}}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Contribute("point", point, id, id)}, nil
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
	accepted, err = frame.ParseBinding(encoded)
	must(err)
	id1, err := original.Identity()
	must(err)
	id2, err := accepted.Identity()
	must(err)
	if id1 != id2 {
		t.Fatal("canonical serialization changed binding identity")
	}
	owner, scope := ownerAndScope(t, host)
	use, err := host.AcquireRecord(ctx, "saved", frame.UseConfig{Owner: owner, Scope: scope})
	must(err)
	snapshot, err := host.Snapshot(ctx, point.Ref())
	must(err)
	candidates, err := frame.Candidates(snapshot, point)
	must(err)
	selected, err := frame.Select(snapshot, point, "all", candidates...)
	must(err)
	_, err = frame.Capture(snapshot, selected.Ref())
	if len(candidates) != 2 || err != nil {
		t.Fatalf("canonical shared binding was rejected: members=%d error=%v", len(candidates), err)
	}
	_, err = use.Close(ctx)
	must(err)
}

func TestBindingIdentitySurvivesPublicJSONRoundTrip(t *testing.T) {
	host := testHost(t, frame.HostConfig{})
	service, err := frame.NewContract[string]("binding.text", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "provider", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "text", Contract: service.Ref()}}},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Provide("text", service, "value")}, nil
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
	for _, unitRoots := range [][]frame.UnitRef{nil, {}} {
		original.UnitRoots = unitRoots
		encoded, err := value.EncodeJSON(original)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := frame.ParseBinding(encoded.Bytes())
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
