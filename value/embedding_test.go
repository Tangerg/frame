package value_test

import (
	"encoding/json/jsontext"
	"reflect"
	"testing"

	"github.com/Tangerg/frame/value"
)

type privateEmbeddedValue struct {
	Value string `json:"value"`
}

type PublicEmbeddedValue struct {
	Value string `json:"value"`
}

type privatePointerEmbedding struct{ *privateEmbeddedValue }
type privateValueEmbedding struct{ privateEmbeddedValue }
type publicPointerEmbedding struct{ *PublicEmbeddedValue }

func TestUnallocatableEmbeddedPointerIsRejectedBeforePublishingItsSchema(t *testing.T) {
	if err := value.ValidateType(reflect.TypeFor[privatePointerEmbedding]()); err == nil {
		t.Fatal("unexported embedded pointer was admitted")
	}
	if _, err := value.SchemaFor[privatePointerEmbedding](); err == nil {
		t.Fatal("published a schema that the fresh typed decoder cannot consume")
	}
	original := privatePointerEmbedding{privateEmbeddedValue: &privateEmbeddedValue{Value: "original"}}
	if err := value.DecodeValue(jsontext.Value(`{"value":"replacement"}`), &original, "input"); err == nil {
		t.Fatal("preinitialized destination bypassed type admission")
	}
	if original.Value != "original" {
		t.Fatal("refused input changed its destination")
	}
}

func TestAllocatableEmbeddedFieldsShareSchemaAndCodec(t *testing.T) {
	for _, test := range []struct {
		name   string
		target reflect.Type
		schema func() ([]byte, error)
	}{
		{"private value", reflect.TypeFor[privateValueEmbedding](), value.SchemaFor[privateValueEmbedding]},
		{"public pointer", reflect.TypeFor[publicPointerEmbedding](), value.SchemaFor[publicPointerEmbedding]},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := value.ValidateType(test.target); err != nil {
				t.Fatal(err)
			}
			descriptor, err := test.schema()
			if err != nil {
				t.Fatal(err)
			}
			schema, err := value.AdmitSchema(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			raw := jsontext.Value(`{"value":"preserved"}`)
			if err := schema.Validate(raw); err != nil {
				t.Fatal(err)
			}
			destination := reflect.New(test.target).Interface()
			if err := value.DecodeValue(raw, destination, "input"); err != nil {
				t.Fatalf("schema admitted a value the decoder refused: %v", err)
			}
			encoded, err := value.EncodeJSON(destination)
			if err != nil || encoded.String() != string(raw) {
				t.Fatalf("embedded value did not round trip: %s, %v", encoded.String(), err)
			}
		})
	}
}
