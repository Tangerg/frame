package value_test

import (
	"encoding/json/jsontext"
	"reflect"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestTypedSchemaRejectsUnrepresentableNullAtConstruction(t *testing.T) {
	type nullable struct {
		Name *string `json:"name" jsonschema:"nullable"`
	}
	type nested struct {
		Entries []map[string]nullable `json:"entries"`
	}
	for _, derive := range []func() ([]byte, error){
		value.SchemaFor[nullable], value.SchemaFor[nested],
	} {
		if _, err := derive(); err == nil {
			t.Fatal("schema published values its typed codec cannot consume")
		}
	}
	if _, err := frame.NewMethod[struct{}, nullable](frame.MethodConfig{Name: "read", Kind: frame.Query}); err == nil {
		t.Fatal("unsupported output type published")
	}
	if _, err := frame.NewStreamingMethod[struct{}, nullable, bool](frame.MethodConfig{Name: "read", Kind: frame.Query}); err == nil {
		t.Fatal("unsupported stream type published")
	}
}

func TestTypedSchemaKeepsOmissionOpaqueNullAndNarrowingConstraints(t *testing.T) {
	type input struct {
		Name    *string `json:"name,omitzero"`
		Payload any     `json:"payload"`
		Count   int     `json:"count,omitzero" jsonschema:"required,minimum=1"`
	}
	rawSchema, err := value.SchemaFor[input]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := value.AdmitSchema(rawSchema)
	if err != nil {
		t.Fatal(err)
	}
	raw := jsontext.Value(`{"payload":null,"count":1.0}`)
	if err := schema.Validate(raw); err != nil {
		t.Fatal(err)
	}
	var decoded input
	if err := value.DecodeValue(raw, &decoded, "input"); err != nil || decoded.Payload != nil || decoded.Count != 1 || decoded.Name != nil {
		t.Fatalf("admitted value did not decode: %+v, %v", decoded, err)
	}
	for _, refused := range []string{`{"payload":null}`, `{"payload":null,"count":0}`, `{"name":null,"payload":null,"count":1}`} {
		if err := schema.Validate([]byte(refused)); err == nil {
			t.Fatalf("narrowed schema admitted %s", refused)
		}
	}
	opaque := reflect.StructOf([]reflect.StructField{{
		Name: "Payload", Type: reflect.TypeFor[any](), Tag: `json:"payload" jsonschema:"nullable"`,
	}})
	if err := value.ValidateType(opaque); err == nil {
		t.Fatal("nullable tag created a second nullability declaration")
	}
}

func TestTypedSchemaPreservesOpaqueJSONNull(t *testing.T) {
	type input struct {
		Payload value.JSON `json:"payload"`
	}
	rawSchema, err := value.SchemaFor[input]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := value.AdmitSchema(rawSchema)
	if err != nil {
		t.Fatal(err)
	}
	raw := jsontext.Value(`{"payload":null}`)
	if err := schema.Validate(raw); err != nil {
		t.Fatal(err)
	}
	var decoded input
	if err := value.DecodeValue(raw, &decoded, "input"); err != nil || decoded.Payload.String() != "null" {
		t.Fatalf("opaque JSON lost null: %s, %v", decoded.Payload.String(), err)
	}
}

func TestTypedSchemaRefusesImplicitNarrowerCodecs(t *testing.T) {
	for _, derive := range []func() ([]byte, error){
		value.SchemaFor[[]byte],
		value.SchemaFor[map[string][]byte],
		value.SchemaFor[time.Time],
		value.SchemaFor[*time.Time],
		value.SchemaFor[float32],
		value.SchemaFor[float64],
	} {
		if _, err := derive(); err == nil {
			t.Fatal("implicit codec published a broader wire contract")
		}
	}
}

func TestTypedSchemaDoesNotInterpretAnnotationTextAsShape(t *testing.T) {
	type input struct {
		Name string `json:"name" jsonschema:"description=literal\\,type=number\\,nullable"`
	}
	if _, err := value.SchemaFor[input](); err != nil {
		t.Fatalf("annotation text changed field admission: %v", err)
	}
}

func TestTypedSchemaRejectsRedundantOpaqueNullModifiers(t *testing.T) {
	type input struct {
		Payload value.JSON `json:"payload" jsonschema:"nullable"`
	}
	if _, err := value.SchemaFor[input](); err == nil {
		t.Fatal("redundant nullable branch published a schema that rejects null")
	}
}
