package value_test

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"reflect"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type wrappedJSON struct{ value.JSON }
type wrappedNumber struct{ value.Number }

type modeledJSON struct{ document value.JSON }

func (v modeledJSON) MarshalJSON() ([]byte, error) { return v.document.MarshalJSON() }
func (v *modeledJSON) UnmarshalJSON(raw []byte) error {
	return v.document.UnmarshalJSON(raw)
}
func (modeledJSON) JSONSchemaModel() any { return jsonv1.RawMessage{} }

type streamingJSON struct{ document value.JSON }

func (v streamingJSON) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteValue(v.document.Bytes())
}
func (v *streamingJSON) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	raw, err := decoder.ReadValue()
	if err != nil {
		return err
	}
	return v.document.UnmarshalJSON(raw)
}
func (streamingJSON) JSONSchemaModel() any { return jsonv1.RawMessage{} }

func modelRoundTrip[T any](t *testing.T, raw, expected string) {
	t.Helper()
	descriptor, err := value.SchemaFor[T]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := value.AdmitSchema(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate([]byte(raw)); err != nil {
		t.Fatalf("schema refused %s: %v", raw, err)
	}
	var decoded T
	if err := value.DecodeValue(jsontext.Value(raw), &decoded, "input"); err != nil {
		t.Fatalf("codec model admitted %s but typed decoding refused it: %v", raw, err)
	}
	encoded, err := value.EncodeJSON(decoded)
	if err != nil || encoded.String() != expected {
		t.Fatalf("decoded value changed %s into %s, want %s: %v", raw, encoded, expected, err)
	}
}

func TestNullableModelsOwnRootAndNestedNull(t *testing.T) {
	t.Run("promoted codec", func(t *testing.T) { modelRoundTrip[wrappedJSON](t, `null`, `null`) })
	t.Run("explicit codec", func(t *testing.T) { modelRoundTrip[modeledJSON](t, `null`, `null`) })
	t.Run("streaming codec", func(t *testing.T) { modelRoundTrip[streamingJSON](t, `null`, `null`) })
	t.Run("required field", func(t *testing.T) {
		modelRoundTrip[struct {
			Payload wrappedJSON `json:"payload"`
		}](t, `{"payload":null}`, `{"payload":null}`)
	})
	t.Run("modeled field", func(t *testing.T) {
		modelRoundTrip[struct {
			Payload modeledJSON `json:"payload"`
		}](t, `{"payload":null}`, `{"payload":null}`)
	})
	t.Run("omitzero field", func(t *testing.T) {
		modelRoundTrip[struct {
			Payload wrappedJSON `json:"payload,omitzero"`
		}](t, `{"payload":null}`, `{"payload":null}`)
	})
	t.Run("absent omitzero field", func(t *testing.T) {
		modelRoundTrip[struct {
			Payload wrappedJSON `json:"payload,omitzero"`
		}](t, `{}`, `{}`)
	})
	t.Run("slice element", func(t *testing.T) { modelRoundTrip[[]wrappedJSON](t, `[null]`, `[null]`) })
	t.Run("map value", func(t *testing.T) { modelRoundTrip[map[string]modeledJSON](t, `{"key":null}`, `{"key":null}`) })
	t.Run("opaque root", func(t *testing.T) { modelRoundTrip[value.JSON](t, `null`, `null`) })
	t.Run("raw message", func(t *testing.T) { modelRoundTrip[jsonv1.RawMessage](t, `null`, `null`) })
	t.Run("raw text", func(t *testing.T) { modelRoundTrip[jsontext.Value](t, `null`, `null`) })
	t.Run("interface", func(t *testing.T) { modelRoundTrip[any](t, `null`, `null`) })
	t.Run("pointer root", func(t *testing.T) { modelRoundTrip[*wrappedJSON](t, `null`, `null`) })
	t.Run("required pointer", func(t *testing.T) {
		modelRoundTrip[struct {
			Payload *wrappedJSON `json:"payload"`
		}](t, `{"payload":null}`, `{"payload":null}`)
	})
	t.Run("pointer slice element", func(t *testing.T) { modelRoundTrip[[]*value.JSON](t, `[null]`, `[null]`) })
	t.Run("pointer map value", func(t *testing.T) { modelRoundTrip[map[string]*value.JSON](t, `{"key":null}`, `{"key":null}`) })
	t.Run("explicit empty omission", func(t *testing.T) {
		modelRoundTrip[struct {
			Payload wrappedJSON `json:"payload,omitempty"`
		}](t, `{"payload":null}`, `{}`)
	})
	t.Run("explicit pointer empty omission", func(t *testing.T) {
		modelRoundTrip[struct {
			Payload *wrappedJSON `json:"payload,omitempty,omitzero"`
		}](t, `{"payload":null}`, `{}`)
	})
}

func rejectLossyType[T any](t *testing.T) {
	t.Helper()
	if err := value.ValidateType(reflect.TypeFor[T]()); err == nil {
		t.Fatal("unrepresentable declaration admitted")
	}
	if _, err := value.SchemaFor[T](); err == nil {
		t.Fatal("unrepresentable declaration published a schema")
	}
	if _, err := frame.NewMethod[struct{}, T](frame.MethodConfig{Name: "read", Kind: frame.Query}); err == nil {
		t.Fatal("unrepresentable output published a method")
	}
	if _, err := frame.NewStreamingMethod[struct{}, T, bool](frame.MethodConfig{Name: "read", Kind: frame.Query}); err == nil {
		t.Fatal("unrepresentable chunk published a stream")
	}
	var decoded T
	if err := value.DecodeValue(jsontext.Value(`{}`), &decoded, "input"); err == nil {
		t.Fatal("invalid declaration reached decoding")
	}
}

func TestOmitZeroCannotCollapseOpaqueNullIntoAbsence(t *testing.T) {
	t.Run("opaque pointer", rejectLossyType[struct {
		Payload *value.JSON `json:"payload,omitzero"`
	}])
	t.Run("promoted pointer", rejectLossyType[struct {
		Payload *wrappedJSON `json:"payload,omitzero"`
	}])
	t.Run("custom pointer", rejectLossyType[struct {
		Payload *streamingJSON `json:"payload,omitzero"`
	}])
	t.Run("raw pointer", rejectLossyType[struct {
		Payload *jsonv1.RawMessage `json:"payload,omitzero"`
	}])
	t.Run("interface", rejectLossyType[struct {
		Payload any `json:"payload,omitzero"`
	}])
	t.Run("pointer to interface", rejectLossyType[struct {
		Payload *any `json:"payload,omitzero"`
	}])
	t.Run("nested", rejectLossyType[[]map[string]struct {
		Payload *value.JSON `json:"payload,omitzero"`
	}])
	t.Run("nonnullable pointer", func(t *testing.T) {
		type input struct {
			Name *string `json:"name,omitzero"`
		}
		modelRoundTrip[input](t, `{}`, `{}`)
		modelRoundTrip[input](t, `{"name":"kept"}`, `{"name":"kept"}`)
	})
}

type privateShape struct{ secret string }
type codecRemoved value.JSON

func TestPrivateRepresentationCannotBecomeAnImplicitEmptyObject(t *testing.T) {
	t.Run("private fields", rejectLossyType[privateShape])
	t.Run("removed codec", rejectLossyType[codecRemoved])
	t.Run("nested", rejectLossyType[struct {
		Child privateShape `json:"child"`
	}])
	original := privateShape{secret: "kept"}
	if err := value.DecodeValue(jsontext.Value(`{}`), &original, "input"); err == nil || original.secret != "kept" {
		t.Fatalf("invalid shape changed its destination: %#v, %v", original, err)
	}
	t.Run("empty struct", func(t *testing.T) { modelRoundTrip[struct{}](t, `{}`, `{}`) })
	t.Run("explicitly ignored field", func(t *testing.T) {
		modelRoundTrip[struct {
			Ignored string `json:"-"`
		}](t, `{}`, `{}`)
	})
}

type opaqueText string

type modelWithoutCodec string

func (modelWithoutCodec) JSONSchemaModel() any { return jsonv1.RawMessage{} }

func (v opaqueText) MarshalText() ([]byte, error) { return []byte(v), nil }
func (v *opaqueText) UnmarshalText(raw []byte) error {
	*v = opaqueText(raw)
	return nil
}
func (opaqueText) JSONSchemaModel() any { return jsonv1.RawMessage{} }

func TestTextCodecCannotClaimAnOpaqueWireModel(t *testing.T) {
	rejectLossyType[opaqueText](t)
}

func TestEncodingModelRequiresTheCodecThatOwnsIt(t *testing.T) {
	rejectLossyType[modelWithoutCodec](t)
}

func TestNullableModelAdmitsMethodOutputsAndChunks(t *testing.T) {
	method, err := frame.NewStreamingMethod[struct{}, streamingJSON, wrappedJSON](frame.MethodConfig{Name: "read", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	if err := method.ValidateOutput(jsontext.Value(`null`)); err != nil {
		t.Fatal(err)
	}
	if err := method.ValidateChunk(jsontext.Value(`null`)); err != nil {
		t.Fatal(err)
	}
}

func rejectTypedNull[T any](t *testing.T) {
	t.Helper()
	descriptor, err := value.SchemaFor[T]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := value.AdmitSchema(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate([]byte(`null`)); err == nil {
		t.Fatal("schema admitted null")
	}
	if value.AllowsNull(reflect.TypeFor[T]()) {
		t.Fatal("encoding model admitted null")
	}
	var decoded T
	if err := value.DecodeValue(jsontext.Value(`null`), &decoded, "input"); err == nil {
		t.Fatal("typed decoding admitted null")
	}
}

func TestNonNullableModelsKeepTheirNullBoundary(t *testing.T) {
	t.Run("string", rejectTypedNull[string])
	t.Run("pointer", rejectTypedNull[*string])
	t.Run("number", rejectTypedNull[value.Number])
	t.Run("promoted number", rejectTypedNull[wrappedNumber])
	t.Run("text codec", rejectTypedNull[value.Digest])
	t.Run("slice", rejectTypedNull[[]value.JSON])
	t.Run("map", rejectTypedNull[map[string]value.JSON])
}
