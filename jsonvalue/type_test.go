package jsonvalue

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"net/netip"
	"reflect"
	"strconv"
	"testing"
	"time"
)

type recursiveEmbedding struct{ *recursiveEmbedding }
type anonymousScalar string
type scalarEmbedding struct{ anonymousScalar }
type recursivePointer *recursivePointer
type mutualPointerLeft *mutualPointerRight
type mutualPointerRight *mutualPointerLeft
type recursiveSlice []recursiveSlice
type recursiveMap map[string]recursiveMap

type RecursiveValue struct {
	Name     string           `json:"name"`
	Children []RecursiveValue `json:"children,omitempty"`
}

type textEncoderOnly struct {
	Value string `json:"value"`
}

func (v *textEncoderOnly) MarshalText() ([]byte, error) { return []byte(v.Value), nil }

type textAppenderOnly textEncoderOnly

func (v textAppenderOnly) AppendText(dst []byte) ([]byte, error) { return append(dst, v.Value...), nil }

type jsonEncoderOnly textEncoderOnly

func (v jsonEncoderOnly) MarshalJSON() ([]byte, error) { return json.Marshal(v.Value) }

type jsonStreamEncoderOnly textEncoderOnly

func (v jsonStreamEncoderOnly) MarshalJSONTo(encoder *jsontext.Encoder) error {
	return encoder.WriteToken(jsontext.String(v.Value))
}

func TestTypeAdmissionRefusesUndecodableDeclarations(t *testing.T) {
	// Construct malformed declarations dynamically so vet still checks real DTOs.
	duplicate := reflect.StructOf([]reflect.StructField{
		{Name: "First", Type: reflect.TypeFor[string](), Tag: `json:"id"`},
		{Name: "Second", Type: reflect.TypeFor[string](), Tag: `json:"id"`},
	})
	nested := reflect.StructOf([]reflect.StructField{{Name: "Child", Type: duplicate, Tag: `json:"child"`}})
	for name, typ := range map[string]reflect.Type{
		"missing type":        nil,
		"duplicate names":     duplicate,
		"invalid nested":      nested,
		"recursive embedding": reflect.TypeFor[recursiveEmbedding](),
		"anonymous scalar":    reflect.TypeFor[scalarEmbedding](),
		"non-string keys":     reflect.TypeFor[map[int]string](),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateType(typ); err == nil {
				t.Fatalf("admitted %v", typ)
			}
		})
	}
	if err := ValidateType(reflect.TypeFor[RecursiveValue]()); err != nil {
		t.Fatalf("structural recursion refused: %v", err)
	}
}

func TestDecodeValuePublishesOnlyCompleteValues(t *testing.T) {
	type input struct {
		Name string   `json:"name"`
		IDs  []string `json:"ids"`
	}
	for _, raw := range []string{
		`{"name":"changed"}`,
		`{"name":"changed","ids":null}`,
		`{"name":"changed","ids":[1]}`,
		`{"name":"changed","ids":[],"unexpected":true}`,
	} {
		value := input{Name: "original", IDs: []string{"preserved"}}
		if err := DecodeValue(jsontext.Value(raw), &value, "params"); err == nil {
			t.Fatalf("admitted %s", raw)
		}
		if !reflect.DeepEqual(value, input{Name: "original", IDs: []string{"preserved"}}) {
			t.Fatalf("refused input changed its destination: %#v", value)
		}
	}
	var value input
	if err := DecodeValue(jsontext.Value(`{"name":"accepted","ids":[]}`), &value, "params"); err != nil || value.Name != "accepted" || value.IDs == nil {
		t.Fatalf("complete value = %#v, %v", value, err)
	}
}

func TestDecodeValueRefusesInvalidDestinationsWithoutPanicking(t *testing.T) {
	for _, target := range []any{nil, 7, (*string)(nil)} {
		if err := DecodeValue(jsontext.Value(`null`), target, "params"); err == nil {
			t.Fatalf("admitted destination %T", target)
		}
	}
}

func TestOpaqueValueOwnsNullSemantics(t *testing.T) {
	var value Value
	if err := DecodeValue(jsontext.Value(`null`), &value, "payload"); err != nil || value.String() != "null" {
		t.Fatalf("opaque null = %s, %v", value.String(), err)
	}
	var typed string
	if err := DecodeValue(jsontext.Value(`null`), &typed, "name"); err == nil {
		t.Fatal("typed null admitted")
	}
}

func TestTypeAdmissionRejectsPointerCyclesBeforeDecoding(t *testing.T) {
	for _, target := range []any{
		new(recursivePointer),
		new(mutualPointerLeft),
		new(struct {
			Pointer recursivePointer `json:"pointer"`
		}),
	} {
		typeOf := reflect.TypeOf(target).Elem()
		if err := ValidateType(typeOf); err == nil {
			t.Fatalf("admitted pointer cycle %v", typeOf)
		}
		if err := DecodeValue(jsontext.Value(`null`), target, "payload"); err == nil {
			t.Fatalf("decoded pointer cycle %v", typeOf)
		}
	}
}

func TestFieldProjectionsRefusePointerCycles(t *testing.T) {
	typeOf := reflect.TypeFor[struct {
		Pointer recursivePointer `json:"pointer"`
	}]()
	for _, projection := range []struct {
		name string
		walk func()
	}{
		{name: "fields", walk: func() { Fields(typeOf) }},
		{name: "embeds", walk: func() { Embeds(typeOf) }},
	} {
		t.Run(projection.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid declaration entered field projection")
				}
			}()
			projection.walk()
		})
	}
}

func TestDecodeValuePreservesStructuralRecursion(t *testing.T) {
	for _, value := range []struct {
		target any
		raw    string
	}{
		{target: new(RecursiveValue), raw: `{"name":"parent","children":[{"name":"child"}]}`},
		{target: new(recursiveSlice), raw: `[[]]`},
		{target: new(recursiveMap), raw: `{"child":{}}`},
	} {
		if err := DecodeValue(jsontext.Value(value.raw), value.target, "payload"); err != nil {
			t.Fatalf("valid recursive value %T refused: %v", value.target, err)
		}
	}
}

func TestTypeAdmissionMatchesAnonymousFieldEncoding(t *testing.T) {
	for _, tag := range []reflect.StructTag{`json:","`, `json:",omitempty"`, `json:",omitzero"`} {
		typeOf := reflect.StructOf([]reflect.StructField{{
			Name: "RecursiveValue", Type: reflect.TypeFor[RecursiveValue](), Anonymous: true, Tag: tag,
		}})
		if err := json.Unmarshal([]byte(`{}`), reflect.New(typeOf).Interface()); err == nil {
			t.Fatalf("standard decoder admitted invalid embedded tag %s", tag)
		}
		if err := ValidateType(typeOf); err == nil {
			t.Fatalf("admitted invalid embedded options %s", tag)
		}
	}
	for _, declaration := range []struct {
		tag reflect.StructTag
		raw string
	}{
		{raw: `{"name":"flattened"}`},
		{tag: `json:""`, raw: `{"name":"flattened"}`},
		{tag: `json:"nested,omitempty"`, raw: `{"nested":{"name":"named"}}`},
	} {
		typeOf := reflect.StructOf([]reflect.StructField{{
			Name: "RecursiveValue", Type: reflect.TypeFor[RecursiveValue](), Anonymous: true, Tag: declaration.tag,
		}})
		value := reflect.New(typeOf)
		if err := DecodeValue(jsontext.Value(declaration.raw), value.Interface(), "payload"); err != nil {
			t.Fatalf("valid field encoding %s refused: %v", declaration.tag, err)
		}
		if value.Elem().Field(0).FieldByName("Name").String() == "" {
			t.Fatalf("field encoding %s lost its value", declaration.tag)
		}
	}
}

func TestTypeAdmissionRejectsMalformedStandardFieldTags(t *testing.T) {
	for _, declaration := range []struct {
		name string
		tag  string
	}{
		{name: "comma only", tag: ","},
		{name: "empty options", tag: ",,"},
		{name: "trailing name delimiter", tag: "value,"},
		{name: "trailing dash delimiter", tag: "-,"},
		{name: "leading empty option", tag: "value,,omitempty"},
		{name: "trailing option delimiter", tag: "value,omitempty,"},
		{name: "interior empty option", tag: "value,omitzero,,omitempty"},
		{name: "duplicate omitempty", tag: "value,omitempty,omitempty"},
		{name: "duplicate omitzero", tag: "value,omitzero,omitzero"},
		{name: "separated duplicate", tag: "value,omitempty,omitzero,omitempty"},
		{name: "quoted name", tag: "'value'"},
		{name: "single quote in name", tag: "val'ue"},
		{name: "double quote in name", tag: "val\"ue"},
		{name: "backslash in name", tag: "val\\ue"},
		{name: "backtick in name", tag: "val`ue"},
		{name: "invalid name UTF-8", tag: "value\xff"},
	} {
		t.Run(declaration.name, func(t *testing.T) {
			typeOf := reflect.StructOf([]reflect.StructField{{
				Name: "Value", Type: reflect.TypeFor[string](), Tag: reflect.StructTag("json:" + strconv.Quote(declaration.tag)),
			}})
			if err := json.Unmarshal([]byte(`{}`), reflect.New(typeOf).Interface()); err == nil {
				t.Fatalf("standard decoder admitted malformed tag %q", declaration.tag)
			}
			if err := ValidateType(typeOf); err == nil {
				t.Fatalf("admitted malformed tag %q", declaration.tag)
			}
		})
	}
}

func TestFieldProjectionMatchesStandardEncoding(t *testing.T) {
	for _, declaration := range []struct {
		name     string
		tag      reflect.StructTag
		wireName string
		optional bool
	}{
		{name: "absent tag", wireName: "Value"},
		{name: "empty tag", tag: `json:""`, wireName: "Value"},
		{name: "plain name", tag: `json:"value"`, wireName: "value"},
		{name: "default omitempty", tag: `json:",omitempty"`, wireName: "Value", optional: true},
		{name: "default omitzero", tag: `json:",omitzero"`, wireName: "Value", optional: true},
		{name: "both omissions", tag: `json:"value,omitempty,omitzero"`, wireName: "value", optional: true},
		{name: "reversed omissions", tag: `json:"value,omitzero,omitempty"`, wireName: "value", optional: true},
		{name: "named dash", tag: `json:"-,omitempty"`, wireName: "-", optional: true},
		{name: "hyphen", tag: `json:"job-id"`, wireName: "job-id"},
		{name: "digits", tag: `json:"0123456789"`, wireName: "0123456789"},
		{name: "spaces", tag: `json:" a b "`, wireName: " a b "},
		{name: "unicode", tag: `json:"世界"`, wireName: "世界"},
		{name: "punctuation", tag: `json:"!#$%&()*+-./:;<=>?@[]^_{|}~ "`, wireName: "!#$%&()*+-./:;<=>?@[]^_{|}~ "},
		{name: "bell", tag: `json:"\a"`, wireName: "\a"},
		{name: "non-printing unicode", tag: `json:"\U0001d173"`, wireName: "\U0001d173"},
	} {
		t.Run(declaration.name, func(t *testing.T) {
			typeOf := reflect.StructOf([]reflect.StructField{{
				Name: "Value", Type: reflect.TypeFor[string](), Tag: declaration.tag,
			}})
			original := reflect.New(typeOf)
			original.Elem().Field(0).SetString("kept")
			encoded, err := json.Marshal(original.Interface())
			if err != nil {
				t.Fatalf("standard encoder rejected valid tag %s: %v", declaration.tag, err)
			}
			var object map[string]string
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			if len(object) != 1 || object[declaration.wireName] != "kept" {
				t.Fatalf("standard field name differs: %s", encoded)
			}
			if err := ValidateType(typeOf); err != nil {
				t.Fatalf("valid tag refused: %v", err)
			}
			fields := Fields(typeOf)
			if len(fields) != 1 || fields[0].Name != declaration.wireName || fields[0].Optional != declaration.optional {
				t.Fatalf("projection = %#v, want name %q, optional %v", fields, declaration.wireName, declaration.optional)
			}
			decoded := reflect.New(typeOf)
			if err := DecodeValue(jsontext.Value(encoded), decoded.Interface(), "payload"); err != nil {
				t.Fatalf("standard frame refused: %v", err)
			}
			if decoded.Elem().Field(0).String() != "kept" {
				t.Fatal("decoded field lost its value")
			}
		})
	}
}

func TestTypeAdmissionKeepsUnsupportedFieldOptionsClosed(t *testing.T) {
	typeOf := reflect.StructOf([]reflect.StructField{{
		Name: "Value", Type: reflect.TypeFor[string](), Tag: `json:"value,futureOption"`,
	}})
	if err := json.Unmarshal([]byte(`{}`), reflect.New(typeOf).Interface()); err != nil {
		t.Fatalf("standard decoder no longer ignores this unknown option: %v", err)
	}
	if err := ValidateType(typeOf); err == nil {
		t.Fatal("unsupported projection option was admitted")
	}
}

func TestDecodeValueHonorsCustomScalarDecoders(t *testing.T) {
	var address netip.Addr
	if err := DecodeValue(jsontext.Value(`"127.0.0.1"`), &address, "address"); err != nil || address != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("text decoder = %v, %v", address, err)
	}
	if err := DecodeValue(jsontext.Value(`null`), &address, "address"); err == nil || address != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("typed null changed the text value: %v, %v", address, err)
	}
	var timestamp time.Time
	if err := DecodeValue(jsontext.Value(`"2026-10-28T00:00:00Z"`), &timestamp, "timestamp"); err != nil || timestamp.IsZero() {
		t.Fatalf("JSON decoder = %v, %v", timestamp, err)
	}
}

func TestTypeAdmissionRefusesEmbeddedCustomEncoders(t *testing.T) {
	for _, embedded := range []reflect.Type{
		reflect.TypeFor[struct{ textEncoderOnly }](),
		reflect.TypeFor[struct{ textAppenderOnly }](),
		reflect.TypeFor[struct{ jsonEncoderOnly }](),
		reflect.TypeFor[struct{ jsonStreamEncoderOnly }](),
	} {
		t.Run(embedded.Field(0).Type.Name(), func(t *testing.T) {
			if err := json.Unmarshal([]byte(`{"value":"kept"}`), reflect.New(embedded).Interface()); err == nil {
				t.Fatal("standard decoder admitted an embedded custom encoder")
			}
			if err := ValidateType(embedded); err == nil {
				t.Fatal("admitted an embedded custom encoder")
			}
			named := reflect.StructOf([]reflect.StructField{{
				Name: "Encoded", Type: embedded.Field(0).Type, Tag: `json:"encoded"`,
			}})
			value := reflect.New(named)
			if err := DecodeValue(jsontext.Value(`{"encoded":{"value":"kept"}}`), value.Interface(), "payload"); err != nil {
				t.Fatalf("explicitly named custom encoder field refused: %v", err)
			}
			if value.Elem().Field(0).FieldByName("Value").String() != "kept" {
				t.Fatal("explicitly named custom encoder field lost its value")
			}
		})
	}
}
