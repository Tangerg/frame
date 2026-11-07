package contractgen

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tangerg/frame/portable"
	framevalue "github.com/Tangerg/frame/value"
)

type projectionSample struct{ name, schema, wire string }

func generateConsumer(t *testing.T, directory string, samples []projectionSample) {
	t.Helper()
	var methods []portable.MethodDescriptor
	for _, sample := range samples {
		methods = append(methods, portable.MethodDescriptor{Name: sample.name, Kind: portable.Query, Description: "Read the declared value.", Input: []byte(`{"type":"object","properties":{"body":{"type":"object"}}}`), Output: []byte(sample.schema)})
	}
	capability, err := portable.NewCapability("test/projection", methods...)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		if sample.wire == "" {
			continue
		}
		method, _ := capability.Method(sample.name)
		if err := method.ValidateOutput([]byte(sample.wire)); err != nil {
			t.Fatalf("original %s rejects sample: %v", sample.name, err)
		}
	}
	data, err := json.Marshal(capability.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "capability.json")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Generate(source, filepath.Join(directory, "schema"), directory, GoOutput{Path: filepath.Join(directory, "types.go"), Package: "consumer"}); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedGoConsumersPreserveAllAdmittedValues(t *testing.T) {
	samples := []projectionSample{
		{"number", `{"type":"number"}`, `9007199254740993`},
		{"integer", `{"type":"integer"}`, `18446744073709551616`},
		{"unsigned", `{"type":"integer","minimum":0.0,"maximum":18446744073709551615}`, `18446744073709551615`},
		{"integerExponent", `{"type":"integer","minimum":0,"maximum":10}`, `1e0`},
		{"negativeZero", `{"type":"integer","minimum":-10,"maximum":10}`, `-0`},
		{"decimal", `{"type":"number"}`, `2.50`},
		{"largeExponent", `{"type":"number"}`, `1e400`},
		{"open", `{"type":"object"}`, `{"memo":"kept"}`},
		{"mixed", `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`, `{"id":"id-1","memo":9007199254740993}`},
		{"nested", `{"type":"object","properties":{"number":{"type":"number"},"body":{"type":"object"},"at":{"type":"string","format":"date-time"}},"required":["number","body","at"],"additionalProperties":false}`, `{"number":2.50,"body":{"memo":"kept"},"at":"2025-01-01T00:00:00.12345678901Z"}`},
		{"nullable", `{"type":["number","null"]}`, `1e400`},
		{"nullableNull", `{"type":["number","null"]}`, `null`},
		{"nullableString", `{"type":["string","null"]}`, `"kept"`},
		{"union", `{"oneOf":[{"type":"number"},{"type":"string"}]}`, `"kept"`},
		{"nullableField", `{"type":"object","properties":{"value":{"type":["number","null"]}},"additionalProperties":false}`, `{"value":null}`},
		{"nullableReferenceField", `{"type":"object","properties":{"value":{"$ref":"#/$defs/Nullable"}},"additionalProperties":false,"$defs":{"Nullable":{"type":["number","null"]}}}`, `{"value":null}`},
		{"opaqueField", `{"type":"object","properties":{"value":{}},"additionalProperties":false}`, `{"value":null}`},
		{"nullField", `{"type":"object","properties":{"value":{"type":"null"}},"additionalProperties":false}`, `{"value":null}`},
		{"absentNullableField", `{"type":"object","properties":{"value":{"type":["number","null"]}},"additionalProperties":false}`, `{}`},
		{"anyValue", `true`, `{"value":null}`},
		{"memberNames", `{"type":"object","properties":{"":{"type":"number"},"-":{"type":"number"},"a,b":{"type":"number"},"a'b":{"type":"number"},"a\"b":{"type":"number"}},"additionalProperties":false}`, `{"":1,"-":2,"a,b":3,"a'b":4,"a\"b":5}`},
		{"unicodeName", `{"type":"object","properties":{"世界":{"type":"string"}},"additionalProperties":false}`, `{"世界":"kept"}`},
	}
	directory := t.TempDir()
	generateConsumer(t, directory, samples)
	var consumer strings.Builder
	consumer.WriteString("package consumer\nimport(\"testing\"; json \"encoding/json/v2\"; \"github.com/Tangerg/frame/value\")\nfunc TestRoundTrip(t *testing.T){\n")
	for _, sample := range samples {
		fmt.Fprintf(&consumer, "{ var decoded %sOutput; wire:=[]byte(%q); if err:=value.DecodeValue(wire,&decoded,\"output\");err!=nil{t.Fatal(err)}; got,err:=json.Marshal(decoded);if err!=nil{t.Fatal(err)}; before,err:=value.ParseJSON(wire);if err!=nil{t.Fatal(err)};after,err:=value.ParseJSON(got);if err!=nil{t.Fatal(err)};if before!=after{t.Fatalf(\"roundtrip changed %%s into %%s\",wire,got)} }\n", goFeatureIdentifier(sample.name), sample.wire)
	}
	consumer.WriteString("}\n")
	test := filepath.Join(directory, "consumer_test.go")
	if err := os.WriteFile(test, []byte(consumer.String()), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "test", filepath.Join(directory, "types.go"), test)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated consumer: %s\n%v", output, err)
	}
}

func TestGeneratedTypeScriptConsumersPreserveShapeConstraints(t *testing.T) {
	samples := []projectionSample{
		{"array", `{"type":"array","items":{"allOf":[{"type":"string"},{"const":"x"}]}}`, `["x"]`},
		{"union", `{"type":"object","properties":{"id":{"type":"string"},"kind":{"type":"string"}},"required":["id"],"oneOf":[{"properties":{"kind":{"const":"a"}},"required":["kind"]},{"properties":{"kind":{"const":"b"}},"required":["kind"]}]}`, `{"id":"id-1","kind":"a"}`},
		{"open", `{"type":"object"}`, `{"memo":"kept"}`},
		{"nested", `{"$defs":{"Problem":{"type":"object","properties":{"detail":{"type":"string"},"type":{"type":"string"}},"required":["type"],"additionalProperties":false}},"$ref":"#/$defs/Problem","allOf":[{"properties":{"type":{"const":"failed"}},"required":["detail"]}]}`, `{"detail":"explanation","type":"failed"}`},
		{"impossible", `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"oneOf":[{"properties":{"id":false}}]}`, ""},
		{"contradictoryLiteral", `{"type":"string","enum":["a"],"const":"b"}`, ""},
		{"forbidden", `false`, ""},
		{"nullableObject", `{"$defs":{"OptionalRecord":{"type":["object","null"],"properties":{"id":{"type":"string"}}}},"$ref":"#/$defs/OptionalRecord","required":["id"]}`, `null`},
		{"requiredAdditional", `{"type":"object","additionalProperties":{"type":"string"},"required":["id"]}`, `{"id":"id-1"}`},
		{"applicator", `{"properties":{"id":{"type":"string"}},"required":["id"]}`, `42`},
	}
	directory := t.TempDir()
	generateConsumer(t, directory, samples)
	source := `import type {ArrayOutput,UnionOutput,OpenOutput,OpenInput,NestedOutput,ImpossibleOutput,ContradictoryLiteralOutput,ForbiddenOutput,NullableObjectOutput,RequiredAdditionalOutput,ApplicatorOutput} from "./methods.generated.js";
const array: ArrayOutput = ["x"];
const union: UnionOutput = {id:"id-1",kind:"a"};
const open: OpenOutput = {memo:"kept"};
const input: OpenInput = {body:{k:1}};
const nested: NestedOutput = {detail:"explanation",type:"failed"};
const nullable: NullableObjectOutput = null;
const nullableRecord: NullableObjectOutput = {id:"id-1"};
const requiredAdditional: RequiredAdditionalOutput = {id:"id-1"};
const applicator: ApplicatorOutput = 42;
// @ts-expect-error The base required property survives each union branch.
const missingId: UnionOutput = {kind:"a"};
// @ts-expect-error An open JSON object still excludes scalar values.
const scalar: OpenOutput = 42;
// @ts-expect-error Both the referenced frame and its narrowing require their fields.
const missingDetail: NestedOutput = {type:"failed"};
// @ts-expect-error A forbidden required field makes this branch unsatisfiable.
const impossible: ImpossibleOutput = {id:"id-1"};
// @ts-expect-error A const does not replace its sibling enum constraint.
const contradictory: ContradictoryLiteralOutput = "b";
// @ts-expect-error A false schema admits no value.
const forbidden: ForbiddenOutput = null;
// @ts-expect-error A nullable reference still constrains its object members.
const missingNullableId: NullableObjectOutput = {};
// @ts-expect-error A required additional property retains its additional schema.
const invalidAdditional: RequiredAdditionalOutput = {id:42};
// @ts-expect-error Applicators without type still constrain object instances.
const missingApplicatorId: ApplicatorOutput = {};
void [array,union,open,input,nested,nullable,nullableRecord,requiredAdditional,applicator,missingId,scalar,missingDetail,impossible,contradictory,forbidden,missingNullableId,invalidAdditional,missingApplicatorId];
`
	if err := os.WriteFile(filepath.Join(directory, "consumer.ts"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	runTypeScriptConsumer(t, directory)
}

func runTypeScriptConsumer(t *testing.T, directory string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	config := struct {
		CompilerOptions map[string]any `json:"compilerOptions"`
		Files           []string       `json:"files"`
	}{
		CompilerOptions: map[string]any{"strict": true, "noEmit": true, "target": "ES2022", "module": "NodeNext", "paths": map[string][]string{"@frame/sdk/json": {filepath.Join(root, "sdk", "src", "json.ts")}}}, Files: []string{"consumer.ts"},
	}
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
		t.Fatalf("generated TypeScript consumer (run npm ci first): %s\n%v", output, err)
	}
}

func TestGenerationResolvesLocalPointersAndPreservesTheirPredicates(t *testing.T) {
	samples := []projectionSample{
		{"escaped", `{"$defs":{"A/B~C":{"type":"string"}},"$ref":"#/$defs/A~1B~0C"}`, `"ok"`},
		{"nested", `{"$defs":{"Container":{"type":"object","properties":{"name":{"type":"string"}}}},"$ref":"#/$defs/Container/properties/name"}`, `"ok"`},
		{"property", `{"type":"object","properties":{"source":{"type":"string"},"alias":{"$ref":"#/properties/source"}},"additionalProperties":false}`, `{"source":"ok","alias":"ok"}`},
		{"legacy", `{"definitions":{"Value":{"type":"string"}},"$ref":"#/definitions/Value"}`, `"ok"`},
		{"percent", `{"$defs":{"sp ace":{"type":"string"}},"$ref":"#/$defs/sp%20ace"}`, `"ok"`},
		{"recursive", `{"type":"array","items":{"$ref":"#"}}`, `[[],[[]]]`},
		{"recursiveObject", `{"type":"object","properties":{"next":{"$ref":"#"}},"required":["next"],"additionalProperties":false}`, ""},
		{"nullableReference", `{"type":["string","null"],"$defs":{"Text":{"type":["string","null"]}},"$ref":"#/$defs/Text"}`, `"ok"`},
	}
	directory := t.TempDir()
	generateConsumer(t, directory, samples)
	data, err := os.ReadFile(filepath.Join(directory, "schema", "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Defs map[string]jsontext.Value `json:"$defs"`
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		raw, err := json.Marshal(struct {
			Ref  string                    `json:"$ref"`
			Defs map[string]jsontext.Value `json:"$defs"`
		}{RefPrefix + goFeatureIdentifier(sample.name) + "Output", bundle.Defs})
		if err != nil {
			t.Fatal(err)
		}
		projected, err := portable.AdmitSchema(raw)
		if err != nil {
			t.Fatalf("%s projected graph: %v", sample.name, err)
		}
		original, err := portable.AdmitSchema([]byte(sample.schema))
		if err != nil {
			t.Fatal(err)
		}
		for _, wire := range []string{sample.wire, `null`, `42`, `"other"`, `{}`, `[]`} {
			if (original.Validate([]byte(wire)) == nil) != (projected.Validate([]byte(wire)) == nil) {
				t.Fatalf("%s changed predicate for %s", sample.name, wire)
			}
		}
	}
	goCheck := exec.CommandContext(t.Context(), "go", "test", filepath.Join(directory, "types.go"))
	if output, err := goCheck.CombinedOutput(); err != nil {
		t.Fatalf("referenced Go consumer: %s %v", output, err)
	}
	ts := `import type * as C from "./methods.generated.js"; const recursive:C.RecursiveOutput=[[],[[]]]; const escaped:C.EscapedOutput="ok"; const nested:C.NestedOutput="ok"; const legacy:C.LegacyOutput="ok"; const nullable:C.NullableReferenceOutput=null; void [recursive,escaped,nested,legacy,nullable];`
	if err := os.WriteFile(filepath.Join(directory, "consumer.ts"), []byte(ts), 0600); err != nil {
		t.Fatal(err)
	}
	runTypeScriptConsumer(t, directory)
}

func TestSharedDefinitionEqualityUsesCanonicalJSON(t *testing.T) {
	first := jsontext.Value(`{"$defs":{"Value":{"type":"string","minLength":1}},"$ref":"#/$defs/Value"}`)
	second := jsontext.Value(`{"$defs":{"Value":{"minLength":1,"type":"string"}},"$ref":"#/$defs/Value"}`)
	before, err := framevalue.ParseJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	after, err := framevalue.ParseJSON(second)
	if err != nil || before != after {
		t.Fatalf("bad test identities: %v", err)
	}
	directory := t.TempDir()
	generateConsumer(t, directory, []projectionSample{{"one", string(first), `"ok"`}, {"two", string(second), `"ok"`}})
	generateConsumer(t, t.TempDir(), []projectionSample{
		{"one", `{"$defs":{"Value":{"const":{"a":1,"b":2}}},"$ref":"#/$defs/Value"}`, `{"a":1,"b":2}`},
		{"two", `{"$defs":{"Value":{"const":{"b":2,"a":1}}},"$ref":"#/$defs/Value"}`, `{"a":1,"b":2}`},
	})
}

func TestExactNumberPublishesItsNumericShape(t *testing.T) {
	method, err := portable.NewMethod[struct{}, framevalue.Number](portable.MethodConfig{Name: "number", Kind: portable.Query, Description: "Read an exact number."})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []string{"2.50", "1e400", "18446744073709551616"} {
		if err := method.ValidateOutput([]byte(sample)); err != nil {
			t.Fatalf("numeric shape rejects %s: %v", sample, err)
		}
	}
	if err := method.ValidateOutput([]byte(`"1"`)); err == nil {
		t.Fatal("numeric shape accepted a string")
	}
}
