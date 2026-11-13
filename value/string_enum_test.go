package value

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func BenchmarkProjectStringEnums(b *testing.B) {
	for _, fields := range []int{64, 256, 1024} {
		b.Run(fmt.Sprint(fields), func(b *testing.B) {
			var raw strings.Builder
			raw.WriteString(`{"type":"object","properties":{`)
			for index := range fields {
				if index != 0 {
					raw.WriteByte(',')
				}
				fmt.Fprintf(&raw, `"field%d":{"type":"string","enum":["approved","denied"],"pattern":"^approved$"}`, index)
			}
			raw.WriteString(`}}`)
			encoded := []byte(raw.String())
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				projections, err := ProjectStringEnums(encoded)
				if err != nil || len(projections) != fields {
					b.Fatalf("projected %d fields: %v", len(projections), err)
				}
			}
		})
	}
}

func TestStringEnumProjectionKeepsTheOriginalSchemaScope(t *testing.T) {
	raw := []byte(`{
		"$defs":{
			"decision/type":{"type":"string","enum":["approved","denied"]},
			"unused":{"type":"string","enum":["2026-11-12","invalid"],"format":"date"}
		},
		"type":"object",
		"properties":{
			"state/name%":{"type":"string","$ref":"#/$defs/decision~1type","not":{"const":"denied"}},
			"impossible":{"type":"string","enum":["denied"],"not":{"const":"denied"}},
			"open":{"type":"string","anyOf":[{"const":"approved"},{"type":"string"}]},
			"union":{"type":"string","anyOf":[{"const":"approved"},{"const":"denied"},{"type":"number"}]},
			"exclusive":{"type":"string","oneOf":[{"enum":["approved","denied"]},{"const":"denied"}]}
		}
	}`)
	projections, err := ProjectStringEnums(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []StringEnumeration{
		{Pointer: "/$defs/decision~1type", Values: []string{"approved", "denied"}},
		{Pointer: "/$defs/unused", Values: []string{"2026-11-12"}},
		{Pointer: "/properties/exclusive", Values: []string{"approved"}},
		{Pointer: "/properties/impossible", Values: []string{}},
		{Pointer: "/properties/state~1name%", Values: []string{"approved"}},
		{Pointer: "/properties/union", Values: []string{"approved", "denied"}},
	}
	if !slices.EqualFunc(projections, want, func(left, right StringEnumeration) bool {
		return left.Pointer == right.Pointer && slices.Equal(left.Values, right.Values) && (left.Values == nil) == (right.Values == nil)
	}) {
		t.Fatalf("enum projections = %#v, want %#v", projections, want)
	}
}

func TestStringEnumProjectionKeepsTheOriginalDocumentBudget(t *testing.T) {
	raw := []byte(`{"type":"string","enum":["approved"],"description":"` + strings.Repeat("x", PortableLimits().MaxDescriptorBytes-128) + `"}`)
	if _, err := AdmitSchema(raw); err != nil {
		t.Fatal(err)
	}
	projections, err := ProjectStringEnums(raw)
	if err != nil || len(projections) != 1 || !slices.Equal(projections[0].Values, []string{"approved"}) {
		t.Fatalf("admitted document was refused during projection: %#v, %v", projections, err)
	}
}

func TestStringEnumProjectionRefusesInvalidDocuments(t *testing.T) {
	for _, raw := range []string{`{"type":"invalid"}`, `{"type":"string","format":"invented"}`, `{"$ref":"#"}`, `{"type":"string","enum":[]}`, `{"type":"string","pattern":"("}`} {
		if _, err := ProjectStringEnums([]byte(raw)); err == nil {
			t.Errorf("invalid schema produced a projection: %s", raw)
		}
	}
}

func FuzzStringEnumProjectionAgreesWithSchema(f *testing.F) {
	for _, raw := range []string{
		`{"type":"string","enum":["approved","denied"],"pattern":"^approved$"}`,
		`{"type":"string","enum":["approved","denied"],"oneOf":[{"const":"approved"},{"const":"denied"}]}`,
		`{"type":"string","allOf":[{"enum":["approved","denied"]},{"not":{"const":"denied"}}]}`,
		`{"type":"string","anyOf":[{"const":"approved"},{"type":"string"}]}`,
		`{"$defs":{"value":{"type":"string","enum":["approved"]}},"type":"string","$ref":"#/$defs/value"}`,
		`{"type":"string","enum":["approved"],"if":{"const":"approved"},"then":false}`,
		`{"type":"string","enum":[""]}`,
		`{"type":["string","null"]}`,
	} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		schema, schemaErr := AdmitSchema(raw)
		projections, err := ProjectStringEnums(raw)
		if schemaErr != nil {
			if err == nil {
				t.Fatal("invalid schema produced an enum projection")
			}
			return
		}
		if errors.Is(err, ErrLimit) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, projection := range projections {
			if projection.Pointer != "" {
				continue
			}
			for _, member := range append(slices.Clone(projection.Values), "", "approved", "denied", "invented", "2026-11-12") {
				encoded, err := json.Marshal(member)
				if err != nil {
					t.Fatal(err)
				}
				admission := schema.Validate(encoded)
				if errors.Is(admission, ErrLimit) {
					continue
				}
				if (admission == nil) != slices.Contains(projection.Values, member) {
					t.Fatalf("projected enum disagrees with schema for %q: %#v", member, projection)
				}
			}
		}
	})
}
