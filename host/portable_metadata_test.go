package host_test

import (
	"context"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestPortableMetadataUsesAdmittedValueShape(t *testing.T) {
	t.Run("plain-integer-control", func(t *testing.T) {
		checkPortableMetadata[struct {
			Count int `json:"count"`
		}](t, `{"count":1}`)
	})
	t.Run("opaque-json-control", func(t *testing.T) {
		checkPortableMetadata[value.JSON](t, `{"payload":null}`)
	})
	t.Run("nullable-interface", func(t *testing.T) {
		checkPortableMetadata[any](t, `null`)
	})
	t.Run("nested-opaque-json", func(t *testing.T) {
		checkPortableMetadata[struct {
			Payload value.JSON `json:"payload"`
		}](t, `{"payload":null}`)
	})
	t.Run("exact-number", func(t *testing.T) {
		checkPortableMetadata[struct {
			Count value.Number `json:"count"`
		}](t, `{"count":9007199254740993}`)
	})
	t.Run("optional-field-present", func(t *testing.T) {
		type metadata struct {
			Label *string `json:"label,omitzero"`
		}
		checkPortableMetadata(t, `{"label":"present"}`, func(value metadata) { *value.Label = "changed" })
	})
	t.Run("optional-field-absent", func(t *testing.T) {
		checkPortableMetadata[struct {
			Label *string `json:"label,omitzero"`
		}](t, `{}`)
	})
}

func TestPortablePointRejectsUnencodableMetadataProjection(t *testing.T) {
	capability, err := frame.NewCapability("test.metadata-projection", frame.MethodDescriptor{
		Name: "read", Kind: frame.Query, Input: []byte(`{"type":"string"}`), Output: []byte(`{"type":"string"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := framehost.NewPortableContract[string]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[value.JSON]("test.metadata-projection", "1", []byte(`true`), service)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := framehost.NewPortablePoint[float64]("test.lossy-projection", "1", []byte(`true`), service); err == nil {
		t.Error("constructor admitted a lossy metadata projection")
	}
	if _, err := framehost.BindPoint[float64, string](point.Ref(), point.Descriptor()); err == nil {
		t.Error("binding admitted a lossy metadata projection")
	}
	if _, err := framehost.BindPoint[chan string, string](point.Ref(), point.Descriptor()); err == nil {
		t.Error("binding admitted a live metadata projection")
	}
}

func checkPortableMetadata[M any](t *testing.T, document string, mutate ...func(M)) {
	t.Helper()
	rawSchema, err := value.SchemaFor[M]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := value.AdmitSchema(rawSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate([]byte(document)); err != nil {
		t.Fatalf("canonical schema refused input: %v", err)
	}
	var decoded M
	if err := value.DecodeValue([]byte(document), &decoded, "metadata"); err != nil {
		t.Fatalf("canonical value decoder refused input: %v", err)
	}
	capability, err := frame.NewCapability("portable.metadata.service", frame.MethodDescriptor{Name: "read", Kind: frame.Query, Input: []byte(`{"type":"string"}`), Output: []byte(`{"type":"string"}`)})
	if err != nil {
		t.Fatal(err)
	}
	service, err := framehost.NewPortableContract[string]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[M]("portable.metadata.point", "1", rawSchema, service)
	if err != nil {
		t.Fatal(err)
	}
	host := testHost(t, framehost.Config{})
	if err := host.Register(service, point); err != nil {
		t.Fatal(err)
	}
	if _, err := host.PrepareMetadata(point.Ref(), []byte(document)); err != nil {
		t.Fatalf("registered portable metadata rejected its own admitted representation: %v", err)
	}

	plugin := installNative(t, host, "metadata", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("member", point, decoded, "callable")}, nil
		},
	})
	ctx := testContext(t)
	if _, err := host.Activate(ctx, source("metadata", plugin, "main", "member", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates=%d", len(candidates))
	}
	check := func(metadata M) {
		t.Helper()
		wire, err := value.EncodeJSON(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if wire.String() != document {
			t.Fatalf("metadata wire=%s, want %s", wire, document)
		}
	}
	read := func() M {
		t.Helper()
		metadata, err := candidates[0].Metadata()
		if err != nil {
			t.Fatal(err)
		}
		return metadata
	}
	check(read())
	for _, change := range mutate {
		change(decoded)
		change(read())
		check(read())
	}
	refs, err := snapshot.Candidates(point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	wire, err := refs[0].MetadataJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != document {
		t.Fatalf("candidate wire=%s, want %s", wire, document)
	}
	selection, err := framehost.Select(snapshot, point, "only-member", candidates...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := framehost.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	_, scope := ownerAndScope(t, host)
	if err := host.With(ctx, scope, plan, func(access *framehost.Access) error {
		member, err := selection.One(access)
		if err != nil {
			return err
		}
		check(member.Metadata)
		if member.Value != "callable" {
			t.Fatalf("value=%q", member.Value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
