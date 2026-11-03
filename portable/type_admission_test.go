package portable_test

import (
	"encoding/json/jsontext"
	"reflect"
	"testing"
	"time"

	"github.com/Tangerg/frame/portable"
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
		portable.SchemaFor[nullable], portable.SchemaFor[nested],
	} {
		if _, err := derive(); err == nil {
			t.Fatal("schema published values its typed codec cannot consume")
		}
	}
	if _, err := portable.NewMethod[struct{}, nullable](portable.MethodConfig{Name: "read", Kind: portable.Query}); err == nil {
		t.Fatal("unsupported output type published")
	}
	if _, err := portable.NewStreamingMethod[struct{}, nullable, bool](portable.MethodConfig{Name: "read", Kind: portable.Query}); err == nil {
		t.Fatal("unsupported stream type published")
	}
}

func TestTypedSchemaKeepsOmissionOpaqueNullAndNarrowingConstraints(t *testing.T) {
	type input struct {
		Name    *string `json:"name,omitzero"`
		Payload any     `json:"payload"`
		Count   int     `json:"count,omitzero" jsonschema:"required,minimum=1"`
	}
	rawSchema, err := portable.SchemaFor[input]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := portable.AdmitSchema(rawSchema)
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
	if err := value.ValidateType(opaque); err != nil {
		t.Fatalf("opaque nullable declaration refused: %v", err)
	}
}

func TestTypedSchemaPreservesOpaqueJSONNull(t *testing.T) {
	type input struct {
		Payload value.JSON `json:"payload"`
	}
	rawSchema, err := portable.SchemaFor[input]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := portable.AdmitSchema(rawSchema)
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
		portable.SchemaFor[[]byte],
		portable.SchemaFor[map[string][]byte],
		portable.SchemaFor[time.Time],
		portable.SchemaFor[*time.Time],
		portable.SchemaFor[float32],
		portable.SchemaFor[float64],
	} {
		if _, err := derive(); err == nil {
			t.Fatal("implicit codec published a broader wire contract")
		}
	}
}

func TestTypedSchemaDoesNotInterpretAnnotationTextAsShape(t *testing.T) {
	type input struct {
		Name string `json:"name" jsonschema:"description=literal\\,type=number"`
	}
	if _, err := portable.SchemaFor[input](); err != nil {
		t.Fatalf("annotation text changed field admission: %v", err)
	}
}
