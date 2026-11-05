package browser

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Tangerg/frame/value"
)

type projectionWireFields struct {
	ID      string        `json:"job-id"`
	Count   int           `json:"count,omitzero"`
	Ignored chan struct{} `json:"-"`
	private func()        //nolint:unused // The projection test asserts that this unexported field never reaches the wire.
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
	}
	_, list, _ := strings.Cut(out.String(), "export const escapedWireValueValues = ")
	list, _, _ = strings.Cut(list, " as const;")
	if err := json.Unmarshal([]byte(list), &values); err != nil {
		t.Fatalf("generated enum values are not JSON strings: %s: %v", list, err)
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

type nestedWireDTO struct {
	Rows    [][]string `json:"rows"`
	Content string     `json:"content"`
	Title   *string    `json:"title"`
}

func TestProjectionMatchesNestedArraysStringsAndRequiredPointers(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[nestedWireDTO]()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"readonly rows: readonly (readonly string[])[];", "readonly content: string;", "readonly title: string;"} {
		if !strings.Contains(out.String(), f) {
			t.Fatalf("missing %q in %s", f, out.String())
		}
	}
	var target nestedWireDTO
	if err := value.DecodeValue([]byte(`{"rows":[],"content":"text","title":null}`), &target, "request"); err == nil {
		t.Fatal("required pointer admitted null")
	}
	if err := value.DecodeValue([]byte(`{"rows":[],"content":"text","title":"title"}`), &target, "request"); err != nil {
		t.Fatal(err)
	}
}

type modeledWireCode struct {
	PrivateValue string `json:"privateValue"`
}

func (modeledWireCode) JSONSchemaModel() any           { return "" }
func (c modeledWireCode) MarshalJSON() ([]byte, error) { return json.Marshal(c.PrivateValue) }
func (c *modeledWireCode) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, &c.PrivateValue)
}

type modeledWireDTO struct {
	Code    modeledWireCode `json:"code"`
	Number  value.Number    `json:"number"`
	Payload jsontext.Value  `json:"payload"`
}

func TestTypeScriptProjectionUsesTheCustomCodecsEncodingModel(t *testing.T) {
	var target modeledWireDTO
	if err := value.DecodeValue([]byte(`{"code":"accepted","number":1.000000000000000000001e400,"payload":null}`), &target, "request"); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[modeledWireDTO]()); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"export type modeledWireCode = string;",
		`export type Number = import("@frame/sdk/json").JSONNumber;`,
		"export type Value = unknown;",
		"readonly code: modeledWireCode;",
	} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %q in %s", expected, out.String())
		}
	}
	if strings.Contains(out.String(), "privateValue") {
		t.Fatalf("private codec representation entered wire declaration: %s", out.String())
	}
}

type nilWireModel struct{ modeledWireCode }
type nilSliceWireModel struct{ modeledWireCode }
type recursiveWireModel struct{ modeledWireCode }
type panickingWireModel struct{ modeledWireCode }
type numericWireEnum int

func (nilWireModel) JSONSchemaModel() any       { return nil }
func (nilSliceWireModel) JSONSchemaModel() any  { return []string(nil) }
func (recursiveWireModel) JSONSchemaModel() any { return recursiveWireModel{} }
func (panickingWireModel) JSONSchemaModel() any { panic(errWireModel) }
func (numericWireEnum) Values() []string        { return []string{"one"} }

var errWireModel = errors.New("wire model failed")

func TestTypeScriptProjectionRejectsInvalidModelsWithoutPublishing(t *testing.T) {
	for _, root := range []reflect.Type{
		reflect.TypeFor[nilWireModel](),
		reflect.TypeFor[nilSliceWireModel](),
		reflect.TypeFor[recursiveWireModel](),
		reflect.TypeFor[panickingWireModel](),
		reflect.TypeFor[numericWireEnum](),
	} {
		var out strings.Builder
		err := WriteTypeScriptTypes(&out, root)
		if err == nil || out.Len() != 0 {
			t.Fatalf("invalid model %s published a projection: %s, %v", root, out.String(), err)
		}
		if root == reflect.TypeFor[panickingWireModel]() && !errors.Is(err, errWireModel) {
			t.Fatalf("model panic lost its cause: %v", err)
		}
	}
}

type nonnullWireJSON struct{ raw jsontext.Value }

func (nonnullWireJSON) JSONSchemaModel() any           { return jsontext.Value{} }
func (v nonnullWireJSON) MarshalJSON() ([]byte, error) { return v.raw, nil }
func (v *nonnullWireJSON) UnmarshalJSON(raw []byte) error {
	v.raw = append(v.raw[:0], raw...)
	return nil
}

func TestTypeScriptProjectionPreservesTheCodecsNullPolicy(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[nonnullWireJSON]()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "export type nonnullWireJSON = {};") {
		t.Fatalf("opaque model changed its codec's null policy: %s", out.String())
	}
	var decoded nonnullWireJSON
	if err := value.DecodeValue([]byte("null"), &decoded, "payload"); err == nil {
		t.Fatal("non-opaque codec admitted null")
	}
	for _, raw := range []string{`"text"`, `42`, `[]`, `{}`} {
		if err := value.DecodeValue([]byte(raw), &decoded, "payload"); err != nil {
			t.Fatalf("codec rejected %s: %v", raw, err)
		}
	}
}

func TestGeneratedTypeScriptConsumerPreservesCodecAndNullContracts(t *testing.T) {
	directory := t.TempDir()
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[nestedWireDTO](), reflect.TypeFor[modeledWireDTO](), reflect.TypeFor[nonnullWireJSON]()); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"types.generated.ts": out.String(),
		"consumer.ts": `import type { nestedWireDTO, modeledWireDTO, nonnullWireJSON } from "./types.generated.js";
const required: nestedWireDTO = {rows: [], content: "text", title: "title"};
const modeled: modeledWireDTO = {code: "code", number: 9007199254740993n, payload: null};
const opaqueValues: nonnullWireJSON[] = ["text", 42, [], {}];
// @ts-expect-error A required pointer cannot encode absence as null.
const nullTitle: nestedWireDTO = {...required, title: null};
// @ts-expect-error The codec's internal storage is not its JSON representation.
const privateCode: modeledWireDTO = {...modeled, code: {privateValue: "code"}};
// @ts-expect-error The numeric codec does not encode its private Go structure.
const privateNumber: modeledWireDTO = {...modeled, number: {document: "1"}};
// @ts-expect-error The model cannot broaden its original codec's null policy.
const opaqueNull: nonnullWireJSON = null;
// @ts-expect-error Undefined has no JSON value spelling.
const opaqueUndefined: nonnullWireJSON = undefined;
void [required, modeled, opaqueValues, nullTitle, privateCode, privateNumber, opaqueNull, opaqueUndefined];
`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	config := struct {
		CompilerOptions struct {
			Strict bool                `json:"strict"`
			NoEmit bool                `json:"noEmit"`
			Target string              `json:"target"`
			Module string              `json:"module"`
			Paths  map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
		Files []string `json:"files"`
	}{Files: []string{"consumer.ts"}}
	config.CompilerOptions.Strict = true
	config.CompilerOptions.NoEmit = true
	config.CompilerOptions.Target = "ES2022"
	config.CompilerOptions.Module = "NodeNext"
	config.CompilerOptions.Paths = map[string][]string{"@frame/sdk/json": {filepath.Join(root, "sdk", "src", "json.ts")}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "tsconfig.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), filepath.Join(root, "node_modules", ".bin", "tsc"), "-p", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated TypeScript consumer: %s\n%v", output, err)
	}
}
