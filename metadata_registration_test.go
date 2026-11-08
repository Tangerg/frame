package frame_test

import (
	"context"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

type wireMetadataDriver struct {
	point    frame.ContractRef
	metadata value.JSON
}

func (wireMetadataDriver) Profile() frame.Profile { return "test.wire-metadata" }
func (d wireMetadataDriver) Prepare(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
	return []frame.ExportValue{{Name: "member", Contract: d.point, Metadata: d.metadata, Value: "original value"}}, nil
}

func (d wireMetadataDriver) ValidateExport(instanceID string, export frame.ExportValue) error {
	if !frame.ValidName(instanceID) || export.Name != "member" || export.Contract != d.point {
		return frame.ErrContractMismatch
	}
	if _, ok := export.Value.(string); !ok {
		return frame.ErrContractMismatch
	}
	return nil
}

func TestPublishedWireMetadataHasNoFrozenGoTarget(t *testing.T) {
	type metadata struct {
		Label *string `json:"label,omitzero"`
	}
	capability, err := portable.NewCapability("example.wire-metadata.value", portable.MethodDescriptor{Name: "read", Kind: portable.Query, Input: []byte(`{"type":"string"}`), Output: []byte(`{"type":"string"}`)})
	if err != nil {
		t.Fatal(err)
	}
	service, err := frame.NewPortableContract[string]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := portable.SchemaFor[metadata]()
	if err != nil {
		t.Fatal(err)
	}
	point, err := frame.NewPortablePoint[metadata]("example.wire-metadata.point", "1", schema, service)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := value.ParseJSON([]byte(`{"label":"original"}`))
	if err != nil {
		t.Fatal(err)
	}
	driver := wireMetadataDriver{point: point.Ref(), metadata: encoded}
	host := testHost(t, frame.HostConfig{Drivers: []frame.Driver{driver}})
	for _, declaration := range []frame.Declaration{service, point} {
		if err := host.RegisterDescriptor(declaration.Ref(), declaration.Descriptor()); err != nil {
			t.Fatal(err)
		}
	}
	plugin := installNative(t, host, "provider", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: driver.Profile(), Exports: []frame.ExportDeclaration{{Name: "member", Contract: point.Ref()}}}})
	if _, err := host.Activate(t.Context(), source("provider", plugin, "main", "member", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(t.Context(), point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service, point); err != nil {
		t.Fatal(err)
	}
	candidates, err := frame.Candidates(snapshot, point)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("local binding invalidated the original snapshot: candidates=%d, error=%v", len(candidates), err)
	}
	first, err := candidates[0].Metadata()
	if err != nil || first.Label == nil || *first.Label != "original" {
		t.Fatalf("typed metadata did not decode its original wire value: %+v, %v", first, err)
	}
	*first.Label = "changed"
	wirePoint, err := frame.BindPoint[value.JSON, string](point.Ref(), point.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	wireCandidates, err := frame.Candidates(snapshot, wirePoint)
	if err != nil || len(wireCandidates) != 1 {
		t.Fatalf("wire view lost the original snapshot: candidates=%d, error=%v", len(wireCandidates), err)
	}
	if wire, err := wireCandidates[0].Metadata(); err != nil || wire.String() != encoded.String() {
		t.Fatalf("wire view changed the original value: %s, %v", wire.String(), err)
	}
	selection, err := frame.Select(snapshot, point, "original", candidates...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := frame.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	_, scope := ownerAndScope(t, host)
	if err := host.With(t.Context(), scope, plan, func(access *frame.Access) error {
		member, err := selection.One(access)
		if err != nil {
			return err
		}
		if member.Metadata.Label == nil || *member.Metadata.Label != "original" || member.Value != "original value" {
			t.Errorf("selected view changed its original wire value: %+v", member)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
