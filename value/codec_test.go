package value_test

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"reflect"
	"testing"

	"github.com/Tangerg/frame/value"
)

type decoderOnly string

func (*decoderOnly) UnmarshalJSON([]byte) error { return nil }
func (decoderOnly) JSONSchemaModel() any        { return "" }

type encoderOnly string

func (v encoderOnly) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }
func (encoderOnly) JSONSchemaModel() any           { return "" }

type unmodeledCodec string

func (v unmodeledCodec) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }
func (v *unmodeledCodec) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, (*string)(v))
}

type pointerModel string

func (v pointerModel) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }
func (v *pointerModel) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, (*string)(v))
}
func (*pointerModel) JSONSchemaModel() any { return "" }

func TestCustomCodecsRequireBothDirectionsAndAnOwnedModel(t *testing.T) {
	for _, sample := range []struct {
		typeOf reflect.Type
		schema func() ([]byte, error)
	}{
		{reflect.TypeFor[decoderOnly](), value.SchemaFor[decoderOnly]},
		{reflect.TypeFor[encoderOnly](), value.SchemaFor[encoderOnly]},
		{reflect.TypeFor[unmodeledCodec](), value.SchemaFor[unmodeledCodec]},
		{reflect.TypeFor[pointerModel](), value.SchemaFor[pointerModel]},
	} {
		t.Run(sample.typeOf.String(), func(t *testing.T) {
			if err := value.ValidateType(sample.typeOf); err == nil {
				t.Fatal("incomplete custom codec admitted")
			}
			if _, err := sample.schema(); err == nil {
				t.Fatal("incomplete custom codec published a schema")
			}
		})
	}
}

func TestOpaqueDecodersPreserveNull(t *testing.T) {
	for _, target := range []any{new(value.JSON), new(jsonv1.RawMessage), new(jsontext.Value), new(any)} {
		if err := value.DecodeValue(jsontext.Value("null"), target, "opaque"); err != nil {
			t.Fatalf("%T lost null: %v", target, err)
		}
		encoded, err := json.Marshal(target)
		if err != nil || string(encoded) != "null" {
			t.Fatalf("%T changed null: %s, %v", target, encoded, err)
		}
	}
}

func TestModeledNumberPreservesValuesBeyondMachineFloats(t *testing.T) {
	schemaJSON, err := value.SchemaFor[value.Number]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := value.AdmitSchema(schemaJSON)
	if err != nil {
		t.Fatal(err)
	}
	const raw = "1.000000000000000000001e400"
	if err := schema.Validate([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	var number value.Number
	if err := value.DecodeValue(jsontext.Value(raw), &number, "number"); err != nil || number.String() != raw {
		t.Fatalf("owned numeric codec lost its model: %s, %v", number.String(), err)
	}
}
