package control

import (
	json "encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type projectionWireFields struct {
	ID      string        `json:"job-id"`
	Count   int           `json:"count,omitzero"`
	Ignored chan struct{} `json:"-"`
	private func()
}

type recursiveProjection struct {
	Children []recursiveProjection `json:"children,omitempty"`
	Parent   *recursiveProjection  `json:"parent,omitempty"`
}

type unsupportedNames []string
type unsupportedGroups map[string]string

type escapedWireValue string

func (escapedWireValue) Values() []string { return []string{"\a", "\U000e0001"} }

type escapedWireFields struct {
	Bell     escapedWireValue `json:"\a"`
	NonASCII string           `json:"\U000e0001"`
}

func TestTypeScriptProjectionPreservesEscapedFieldAndEnumValues(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[escapedWireFields]()); err != nil {
		t.Fatal(err)
	}
	var fields, values []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if field, ok := strings.CutPrefix(line, "readonly "); ok {
			literal, _, found := strings.Cut(field, ":")
			if !found {
				t.Fatalf("malformed generated field: %s", line)
			}
			var decoded string
			if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
				t.Fatalf("generated property is not a JSON string: %s: %v", literal, err)
			}
			fields = append(fields, decoded)
		}
		if declaration, ok := strings.CutPrefix(line, "export type escapedWireValue = "); ok {
			for literal := range strings.SplitSeq(strings.TrimSuffix(declaration, ";"), " | ") {
				var decoded string
				if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
					t.Fatalf("generated enum member is not a JSON string: %s: %v", literal, err)
				}
				values = append(values, decoded)
			}
		}
	}
	want := []string{"\a", "\U000e0001"}
	if !slices.Equal(fields, want) || !slices.Equal(values, want) {
		t.Fatalf("projection changed field names or enum values: fields %q, values %q\n%s", fields, values, out.String())
	}
}

func TestTypeScriptProjectionUsesJSONFieldOwnership(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[projectionWireFields]()); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`readonly "job-id": string;`, "readonly count?: number;"} {
		if !strings.Contains(out.String(), field) {
			t.Fatalf("missing field %q in %s", field, out.String())
		}
	}
	if strings.Contains(out.String(), "Ignored") || strings.Contains(out.String(), "private") {
		t.Fatalf("non-wire fields entered the projection: %s", out.String())
	}
}

func TestTypeScriptProjectionKeepsRecursiveStructReferences(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[recursiveProjection]()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "export interface recursiveProjection {") != 1 {
		t.Fatalf("recursive owner was not emitted once: %s", out.String())
	}
	for _, field := range []string{
		"readonly children?: readonly recursiveProjection[];",
		"readonly parent?: recursiveProjection;",
	} {
		if !strings.Contains(out.String(), field) {
			t.Fatalf("missing recursive field %q in %s", field, out.String())
		}
	}
}

func TestTypeScriptProjectionRejectsInvalidOrUnsupportedRoots(t *testing.T) {
	for _, root := range []reflect.Type{
		nil,
		reflect.TypeFor[func()](),
		reflect.TypeFor[map[int]string](),
		reflect.TypeFor[unsupportedNames](),
		reflect.TypeFor[unsupportedGroups](),
	} {
		var out strings.Builder
		if err := WriteTypeScriptTypes(&out, root); err == nil {
			t.Fatalf("admitted projection root %v: %s", root, out.String())
		}
		if out.Len() != 0 {
			t.Fatalf("refused root %v published partial output: %s", root, out.String())
		}
	}
}
