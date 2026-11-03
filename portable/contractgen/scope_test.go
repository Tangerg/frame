package contractgen

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tangerg/frame/portable"
)

func TestGeneratedConsumersPreserveIndependentSchemaScopes(t *testing.T) {
	methods := []portable.MethodDescriptor{
		{
			Name: "first", Kind: portable.Query, Description: "Read separate method frames.",
			Input:  []byte(`{"$defs":{"domain.Item":{"type":"string"}},"$ref":"#/$defs/domain.Item"}`),
			Output: []byte(`{"$defs":{"domain.Item":{"type":"number"}},"$ref":"#/$defs/domain.Item"}`),
			Stream: []byte(`{"$defs":{"domain.Item":{"type":"boolean"}},"$ref":"#/$defs/domain.Item"}`),
		},
		{
			Name: "second", Kind: portable.Query, Description: "Read a local recursive value.",
			Input:  []byte(`{"type":"object","additionalProperties":false}`),
			Output: []byte(`{"$defs":{"domain.Item":{"type":"array","items":{"$ref":"#/$defs/domain.Item"}}},"$ref":"#/$defs/domain.Item"}`),
		},
		{
			Name: "third", Kind: portable.Query, Description: "Read a text reference.",
			Input:  []byte(`{"type":"object","additionalProperties":false}`),
			Output: []byte(`{"$defs":{"Item":{"$ref":"#/properties/value"}},"type":"object","properties":{"value":{"type":"string"},"copy":{"$ref":"#/$defs/Item"}},"required":["value","copy"],"additionalProperties":false}`),
		},
		{
			Name: "fourth", Kind: portable.Query, Description: "Read a boolean reference.",
			Input:  []byte(`{"type":"object","additionalProperties":false}`),
			Output: []byte(`{"$defs":{"Item":{"$ref":"#/properties/value"}},"type":"object","properties":{"value":{"type":"boolean"},"copy":{"$ref":"#/$defs/Item"}},"required":["value","copy"],"additionalProperties":false}`),
		},
	}
	descriptor, err := featureDescriptor(methods...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := portable.ParseCapability(descriptor); err != nil {
		t.Fatalf("original descriptor: %v", err)
	}
	directory := t.TempDir()
	source := filepath.Join(directory, "source.json")
	if err := os.WriteFile(source, descriptor, 0600); err != nil {
		t.Fatal(err)
	}
	goOutput := GoOutput{Path: filepath.Join(directory, "types.go"), Package: "consumer"}
	if err := Generate(source, directory, directory, goOutput); err != nil {
		t.Fatalf("independently admitted schema documents: %v", err)
	}
	var bundle struct {
		Defs map[string]jsontext.Value `json:"$defs"`
	}
	data, err := os.ReadFile(filepath.Join(directory, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(directory, "methods.generated.json"))
	if err != nil {
		t.Fatal(err)
	}
	var projectedMethods []featureMethod
	if err := json.Unmarshal(data, &projectedMethods); err != nil {
		t.Fatal(err)
	}
	for _, method := range methods {
		var projected featureMethod
		for _, candidate := range projectedMethods {
			if candidate.Name == method.Name {
				projected = candidate
				break
			}
		}
		for _, frame := range []struct {
			raw jsontext.Value
			ref string
		}{{method.Input, projected.Input}, {method.Output, projected.Output}, {method.Stream, projected.Stream}} {
			if len(frame.raw) == 0 {
				continue
			}
			before, err := portable.AdmitSchema(frame.raw)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(struct {
				Defs map[string]jsontext.Value `json:"$defs"`
				Ref  string                    `json:"$ref"`
			}{bundle.Defs, strings.TrimPrefix(frame.ref, "schema.json")})
			if err != nil {
				t.Fatal(err)
			}
			after, err := portable.AdmitSchema(raw)
			if err != nil {
				t.Fatalf("projected %s: %v", frame.ref, err)
			}
			for _, wire := range []string{`null`, `"text"`, `42`, `true`, `[]`, `[[],[[]]]`, `{}`, `{"value":"text","copy":"text"}`, `{"value":true,"copy":true}`, `{"value":"text","copy":true}`} {
				if (before.Validate([]byte(wire)) == nil) != (after.Validate([]byte(wire)) == nil) {
					t.Fatalf("%s changed its local reference target for %s", frame.ref, wire)
				}
			}
		}
	}
	generated := make(map[string][]byte)
	for _, name := range []string{"schema.json", "methods.generated.ts", "types.go"} {
		generated[name], err = os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := Generate(source, directory, directory, goOutput); err != nil {
		t.Fatal(err)
	}
	for name, original := range generated {
		repeated, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || !bytes.Equal(original, repeated) {
			t.Fatalf("document naming changed on repeated generation for %s: %v", name, err)
		}
	}
	goConsumer := `package consumer
import ("testing"; json "encoding/json/v2")
func TestScopedValues(t *testing.T) {
 for _,sample:=range []struct{wire string;value any}{
  {"\"text\"",new(FirstInputDomainItem)},
  {"42",new(FirstOutputDomainItem)},
  {"true",new(FirstChunkDomainItem)},
  {"[[],[[]]]",new(SecondOutputDomainItem)},
  {"{\"copy\":\"text\",\"value\":\"text\"}",new(ThirdOutput)},
  {"{\"copy\":true,\"value\":true}",new(FourthOutput)},
 } {
  if err:=json.Unmarshal([]byte(sample.wire),sample.value);err!=nil{t.Fatal(err)}
  encoded,err:=json.Marshal(sample.value,json.Deterministic(true))
  if err!=nil||string(encoded)!=sample.wire{t.Fatalf("%s became %s: %v",sample.wire,encoded,err)}
 }
}
`
	test := filepath.Join(directory, "consumer_test.go")
	if err := os.WriteFile(test, []byte(goConsumer), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "test", goOutput.Path, test)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated Go scopes: %s\n%v", output, err)
	}
	typeScriptConsumer := `import type {MethodInputs,MethodOutputs,MethodChunks} from "./methods.generated.js";
const input: MethodInputs["first"] = "text";
const output: MethodOutputs["first"] = 42;
const chunk: MethodChunks["first"] = true;
const recursive: MethodOutputs["second"] = [[],[[]]];
const text: MethodOutputs["third"] = {value:"text",copy:"text"};
const boolean: MethodOutputs["fourth"] = {value:true,copy:true};
// @ts-expect-error Input and output retain different local Item definitions.
const wrongInput: MethodInputs["first"] = 42;
// @ts-expect-error A chunk definition cannot replace its output definition.
const wrongOutput: MethodOutputs["first"] = true;
// @ts-expect-error Equal reference spellings resolve in their original document.
const wrongReference: MethodOutputs["fourth"] = {value:true,copy:"text"};
void [input,output,chunk,recursive,text,boolean,wrongInput,wrongOutput,wrongReference];
`
	if err := os.WriteFile(filepath.Join(directory, "consumer.ts"), []byte(typeScriptConsumer), 0600); err != nil {
		t.Fatal(err)
	}
	runTypeScriptConsumer(t, directory)
}
