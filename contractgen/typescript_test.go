package contractgen

import (
	json "encoding/json/v2"
	"testing"
)

func TestTypeScriptNestedNarrowingPreservesRequiredFieldsAndLiteralTypes(t *testing.T) {
	definitions := Definitions{
		"Problem": {
			Type: SchemaTypeObject,
			Properties: map[string]any{
				"detail": &Schema{Type: SchemaTypeString},
				"type":   &Schema{Ref: RefPrefix + "ProblemType"},
			},
			Required: []string{"type"},
		},
		"ProblemType": {Enum: []string{"timeout", "toolFailed"}},
	}
	emitter := NewTypeScript(definitions)

	got := emitter.narrow(
		&Schema{Ref: RefPrefix + "Problem"},
		&Schema{
			Properties: map[string]any{"type": &Schema{Enum: []string{"toolFailed"}}},
			Required:   []string{"detail"},
		},
	)
	const want = `{ detail: string; type: "toolFailed" }`
	if got != want {
		t.Fatalf("nested narrowing = %s, want %s", got, want)
	}
}

func TestTypeScriptStringLiteralsPreserveJSONValues(t *testing.T) {
	emitter := NewTypeScript(nil)
	for _, value := range []struct {
		name string
		text string
	}{
		{name: "bell", text: "\a"},
		{name: "zero", text: "\x00"},
		{name: "vertical tab", text: "\v"},
		{name: "supplementary format character", text: "\U000e0001"},
		{name: "line separators", text: "\u2028\u2029"},
		{name: "whitespace", text: "\r\n\t"},
		{name: "quotes and backslash", text: "'quoted\"\\path"},
		{name: "unicode", text: "世界"},
		{name: "empty", text: ""},
	} {
		t.Run(value.name, func(t *testing.T) {
			literals := []string{
				TypeScriptStringLiteral(value.text),
				PropertyKey(value.text),
				QuoteAll([]string{value.text})[0],
				emitter.TypeOf(&Schema{Enum: []string{value.text}}),
			}
			if value.text != "" {
				literals = append(literals, emitter.narrow(&Schema{Type: SchemaTypeString}, &Schema{Const: value.text}))
			}
			for _, literal := range literals {
				var decoded string
				if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
					t.Fatalf("generated literal %q is not a JSON string: %v", literal, err)
				}
				if decoded != value.text {
					t.Fatalf("generated literal %q changed %q into %q", literal, value.text, decoded)
				}
			}
		})
	}
	for _, name := range []string{"id", "$name", "_state", "name_2"} {
		if got := PropertyKey(name); got != name {
			t.Fatalf("identifier property %q changed to %q", name, got)
		}
	}
}

func TestTypeScriptStringEmissionRefusesInvalidUnicode(t *testing.T) {
	const invalid = "\xff"
	for _, emission := range []struct {
		name string
		call func()
	}{
		{name: "literal", call: func() { TypeScriptStringLiteral(invalid) }},
		{name: "property", call: func() { PropertyKey(invalid) }},
		{name: "enum", call: func() { NewTypeScript(nil).TypeOf(&Schema{Enum: []string{invalid}}) }},
	} {
		t.Run(emission.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid Unicode entered TypeScript output")
				}
			}()
			emission.call()
		})
	}
}
