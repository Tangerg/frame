package browser

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"math"
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
	for _, field := range []string{`readonly "job-id": string;`, `readonly count?: import("@frame/sdk/json").JSONNumber;`} {
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

type nullableWireJSON struct{ raw jsontext.Value }

func (nullableWireJSON) JSONSchemaModel() any           { return jsontext.Value{} }
func (v nullableWireJSON) MarshalJSON() ([]byte, error) { return v.raw, nil }
func (v *nullableWireJSON) UnmarshalJSON(raw []byte) error {
	v.raw = append(v.raw[:0], raw...)
	return nil
}

func TestTypeScriptProjectionPreservesTheCodecsNullPolicy(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[nullableWireJSON]()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "export type nullableWireJSON = unknown;") {
		t.Fatalf("opaque model changed its codec's null policy: %s", out.String())
	}
	var decoded nullableWireJSON
	if err := value.DecodeValue([]byte("null"), &decoded, "payload"); err != nil {
		t.Fatalf("opaque codec rejected null: %v", err)
	}
	for _, raw := range []string{`"text"`, `42`, `[]`, `{}`} {
		if err := value.DecodeValue([]byte(raw), &decoded, "payload"); err != nil {
			t.Fatalf("codec rejected %s: %v", raw, err)
		}
	}
}

func TestGeneratedTypeScriptConsumerPreservesCodecAndNullContracts(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[nestedWireDTO](), reflect.TypeFor[modeledWireDTO](), reflect.TypeFor[nullableWireJSON]()); err != nil {
		t.Fatal(err)
	}
	checkTypeScriptConsumer(t, out.String(), `import type { nestedWireDTO, modeledWireDTO, nullableWireJSON } from "./types.generated.js";
const required: nestedWireDTO = {rows: [], content: "text", title: "title"};
const modeled: modeledWireDTO = {code: "code", number: 9007199254740993n, payload: null};
const opaqueValues: nullableWireJSON[] = ["text", 42, [], {}, null];
// @ts-expect-error A required pointer cannot encode absence as null.
const nullTitle: nestedWireDTO = {...required, title: null};
// @ts-expect-error The codec's internal storage is not its JSON representation.
const privateCode: modeledWireDTO = {...modeled, code: {privateValue: "code"}};
// @ts-expect-error The numeric codec does not encode its private Go structure.
const privateNumber: modeledWireDTO = {...modeled, number: {document: "1"}};
void [required, modeled, opaqueValues, nullTitle, privateCode, privateNumber];
`)
}

func checkTypeScriptConsumer(t *testing.T, generated, consumer string) {
	t.Helper()
	directory := t.TempDir()
	for name, source := range map[string]string{"types.generated.ts": generated, "consumer.ts": consumer} {
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
		t.Fatalf("generated TypeScript consumer: %s\n%v\n%s", output, err, generated)
	}
}

type integerWireDTO struct {
	Signed         int64  `json:"signed"`
	Unsigned       uint64 `json:"unsigned"`
	Native         int    `json:"native"`
	NativeUnsigned uint   `json:"nativeUnsigned"`
	Small          int32  `json:"small"`
	SmallUnsigned  uint32 `json:"smallUnsigned"`
}

func TestTypeScriptIntegerProjectionPreservesWideAndSafeValues(t *testing.T) {
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[integerWireDTO]()); err != nil {
		t.Fatal(err)
	}
	checkTypeScriptConsumer(t, out.String(), `import type { integerWireDTO } from "./types.generated.js";
import { ExactNumber } from "@frame/sdk/json";
const wide: integerWireDTO = {signed: -9223372036854775808n, unsigned: 18446744073709551615n, native: 9007199254740993n, nativeUnsigned: 9007199254740993n, small: -2147483648, smallUnsigned: 4294967295};
const safe: integerWireDTO = {...wide, signed: 42, unsigned: 42, native: 42, nativeUnsigned: 42};
const exponent: integerWireDTO = {...wide, signed: new ExactNumber("9007199254740993.0")};
// @ts-expect-error Wide integers remain numeric wire values, not strings.
const text: integerWireDTO = {...wide, unsigned: "18446744073709551615"};
// @ts-expect-error A fixed-width safe integer retains its JavaScript number representation.
const bigintSmall: integerWireDTO = {...wide, small: 1n};
// @ts-expect-error Numeric projection cannot introduce null.
const nullWide: integerWireDTO = {...wide, signed: null};
void [wide, safe, exponent, text, bigintSmall, nullWide];
`)
	encoded, err := value.EncodeJSON(integerWireDTO{Signed: math.MinInt64, Unsigned: math.MaxUint64, Native: 42, NativeUnsigned: 42, Small: math.MinInt32, SmallUnsigned: math.MaxUint32})
	if err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs("../sdk/src/json.ts")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "node", "--input-type=module", "-e", `
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
const { parseJSON } = await import(pathToFileURL(process.argv[1]));
const value = parseJSON(readFileSync(0, "utf8"));
assert.deepEqual(value, {signed: -9223372036854775808n, unsigned: 18446744073709551615n, native: 42, nativeUnsigned: 42, small: -2147483648, smallUnsigned: 4294967295});
`, module)
	command.Stdin = strings.NewReader(encoded.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated integer DTO does not match SDK decoding: %s\n%v", output, err)
	}
}

type genericWireEnvelope[T any] struct {
	Value    T                        `json:"value"`
	Children []genericWireEnvelope[T] `json:"children,omitempty"`
}

type genericWireDTO struct {
	Text   genericWireEnvelope[string]                      `json:"text"`
	Count  genericWireEnvelope[int32]                       `json:"count"`
	Nested genericWireEnvelope[genericWireEnvelope[string]] `json:"nested"`
}

func TestTypeScriptProjectionNamesGenericDeclarationsAndReferences(t *testing.T) {
	roots := []reflect.Type{reflect.TypeFor[genericWireDTO](), reflect.TypeFor[genericWireEnvelope[string]]()}
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, roots...); err != nil {
		t.Fatal(err)
	}
	checkTypeScriptConsumer(t, out.String(), `import type { genericWireDTO, genericWireEnvelope_string, genericWireEnvelope_int32 } from "./types.generated.js";
const text: genericWireEnvelope_string = {value: "text", children: [{value: "child"}]};
const count: genericWireEnvelope_int32 = {value: 42};
const values: genericWireDTO = {text, count, nested: {value: text}};
// @ts-expect-error Independent instantiations retain their own value types.
const swapped: genericWireDTO = {...values, text: count};
// @ts-expect-error Recursive references retain their instantiated type.
const wrongChild: genericWireEnvelope_string = {value: "text", children: [count]};
void [text, count, values, swapped, wrongChild];
`)
	slices.Reverse(roots)
	var reordered strings.Builder
	if err := WriteTypeScriptTypes(&reordered, roots...); err != nil {
		t.Fatal(err)
	}
	if reordered.String() != out.String() {
		t.Fatal("root ordering changed the generated names")
	}
}

type genericWireEnvelope_string struct {
	Value bool `json:"value"`
}

func TestTypeScriptProjectionRejectsCollidingGenericNamesBeforePublication(t *testing.T) {
	roots := []reflect.Type{reflect.TypeFor[genericWireEnvelope[string]](), reflect.TypeFor[genericWireEnvelope_string]()}
	for range 2 {
		var out strings.Builder
		if err := WriteTypeScriptTypes(&out, roots...); err == nil || out.Len() != 0 {
			t.Fatalf("colliding projected names published output: %s, %v", out.String(), err)
		}
		slices.Reverse(roots)
	}
}

type enum string

func (enum) Values() []string { return []string{"ready", "closed"} }

type enum_Values struct {
	Label string `json:"label"`
}

func TestTypeScriptProjectionPreservesLegalDeclarationNamespaces(t *testing.T) {
	type number int32
	type await struct {
		Value string `json:"value"`
	}
	type private struct {
		Value string `json:"value"`
	}
	type async struct {
		Value string `json:"value"`
	}
	type readonly struct {
		Value string `json:"value"`
	}
	type infer int32
	type keyof int32
	type unique int32
	type declarationNames struct {
		State    enum     `json:"state"`
		Count    number   `json:"count"`
		Waiting  await    `json:"waiting"`
		Hidden   private  `json:"hidden"`
		Async    async    `json:"async"`
		Readonly readonly `json:"readonly"`
		Infer    infer    `json:"infer"`
		Keyof    keyof    `json:"keyof"`
		Unique   unique   `json:"unique"`
	}
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[declarationNames](), reflect.TypeFor[enum_Values]()); err != nil {
		t.Fatal(err)
	}
	checkTypeScriptConsumer(t, out.String(), `import { enum_Values, type enum_, type number_, type await_, type private_, type async, type readonly_, type declarationNames, type enum_Values as ValuesDTO } from "./types.generated.js";
const state: enum_ = enum_Values[0];
const listProjection: ValuesDTO = {label: "independent type namespace"};
const count: number_ = 42;
const waiting: await_ = {value: "waiting"};
const hidden: private_ = {value: "hidden"};
const contextual: async = {value: "async"};
const readable: readonly_ = {value: "readonly"};
const composed: declarationNames = {state, count, waiting, hidden, async: contextual, readonly: readable, infer: 1, keyof: 2, unique: 3};
// @ts-expect-error Target names preserve the original numeric shape.
const wrongCount: number_ = "42";
// @ts-expect-error The type sharing the enum list's value name remains independent.
const wrongListProjection: ValuesDTO = enum_Values;
void [state, listProjection, count, waiting, hidden, contextual, readable, composed, wrongCount, wrongListProjection];
`)
}

func TestTypeScriptProjectionRejectsReservedNameCollisionsBeforePublication(t *testing.T) {
	type number int32
	type number_ string
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[number](), reflect.TypeFor[number_]()); err == nil || out.Len() != 0 {
		t.Fatalf("reserved declaration name collided after publication: %s, %v", out.String(), err)
	}
}

func TestTypeScriptProjectionPreservesContextualNames(t *testing.T) {
	type as string
	type asserts string
	type is string
	type satisfies string
	type intrinsic string
	type namespace string
	type contextualNames struct {
		Alias     as        `json:"alias"`
		Assertion asserts   `json:"assertion"`
		Predicate is        `json:"predicate"`
		Satisfied satisfies `json:"satisfied"`
		Intrinsic intrinsic `json:"intrinsic"`
		Namespace namespace `json:"namespace"`
	}
	var out strings.Builder
	if err := WriteTypeScriptTypes(&out, reflect.TypeFor[contextualNames]()); err != nil {
		t.Fatal(err)
	}
	checkTypeScriptConsumer(t, out.String(), `import type { as_, asserts, is, satisfies, intrinsic, namespace, contextualNames } from "./types.generated.js";
const alias: as_ = "alias";
const assertion: asserts = "assertion";
const predicate: is = "predicate";
const satisfied: satisfies = "satisfied";
const intrinsicValue: intrinsic = "intrinsic";
const namespaceValue: namespace = "namespace";
const values: contextualNames = {alias, assertion, predicate, satisfied, intrinsic: intrinsicValue, namespace: namespaceValue};
// @ts-expect-error Renaming retains the original string shape.
const invalid: as_ = 42;
void [values, invalid];
`)
}

func TestTypeScriptProjectionRejectsContextualNameCollisionsBeforePublication(t *testing.T) {
	type as string
	type as_ bool
	roots := []reflect.Type{reflect.TypeFor[as](), reflect.TypeFor[as_]()}
	for range 2 {
		var out strings.Builder
		if err := WriteTypeScriptTypes(&out, roots...); err == nil || out.Len() != 0 {
			t.Fatalf("contextual name collision published output: %s, %v", out.String(), err)
		}
		slices.Reverse(roots)
	}
}
