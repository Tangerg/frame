package frame_test

import (
	"context"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
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

func checkPortableMetadata[M any](t *testing.T, document string, mutate ...func(M)) {
	t.Helper()
	rawSchema, err := portable.SchemaFor[M]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := portable.AdmitSchema(rawSchema)
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
	capability, err := portable.NewCapability("portable.metadata.service", portable.MethodDescriptor{Name: "read", Kind: portable.Query, Input: []byte(`{"type":"string"}`), Output: []byte(`{"type":"string"}`)})
	if err != nil {
		t.Fatal(err)
	}
	service, err := frame.NewPortableContract[string]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	point, err := frame.NewPortablePoint[M]("portable.metadata.point", "1", rawSchema, service)
	if err != nil {
		t.Fatal(err)
	}
	host := testHost(t, frame.HostConfig{})
	if err := host.Register(service, point); err != nil {
		t.Fatal(err)
	}
	if _, err := host.DecodeMetadata(point.Ref(), []byte(document)); err != nil {
		t.Fatalf("registered portable metadata rejected its own admitted representation: %v", err)
	}

	plugin := installNative(t, host, "metadata", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Contribute("member", point, decoded, "callable")}, nil
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
	candidates, err := frame.Candidates(snapshot, point)
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
	selection, err := frame.Select(snapshot, point, "only-member", candidates...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := frame.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	_, scope := ownerAndScope(t, host)
	if err := host.With(ctx, scope, plan, func(access *frame.Access) error {
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
