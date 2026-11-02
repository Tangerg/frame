package contractgen

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedNamespacesPreserveLegalContractNames(t *testing.T) {
	definitions := map[string]jsontext.Value{
		"MethodInputs": []byte(`{"type":"string"}`), "MethodOutputs": []byte(`{"type":"string"}`),
		"MethodChunks": []byte(`{"type":"string"}`), "JSONNumber": []byte(`{"type":"string"}`),
		"METHODS": []byte(`{"type":"string"}`), "CAPABILITY": []byte(`{"type":"string"}`),
		"CAPABILITY_JSON": []byte(`{"type":"string"}`), "CapabilityJSON": []byte(`{"type":"string"}`),
		"ReadMethod": []byte(`{"type":"string"}`), "Array": []byte(`{"type":"array","items":{"type":"number"}}`),
		"Record": []byte(`{"type":"object","additionalProperties":{"type":"string"}}`),
	}
	properties := make(map[string]any, len(definitions))
	for name := range definitions {
		properties[name] = map[string]string{"$ref": RefPrefix + name}
	}
	schema, err := json.Marshal(map[string]any{"$defs": definitions, "type": "object", "properties": properties, "additionalProperties": false})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	var samples []projectionSample
	for _, name := range []string{"__proto__", "constructor", "toString", "first/read", "second/read"} {
		samples = append(samples, projectionSample{name: name, schema: string(schema)})
	}
	generateConsumer(t, directory, samples)
	source := `import type {MethodInputs,MethodOutputs} from "./methods.generated.js";
const input: MethodInputs["__proto__"] = {body:{}};
const output: MethodOutputs["__proto__"] = {JSONNumber:"named",MethodInputs:"input",MethodOutputs:"output",MethodChunks:"chunk",METHODS:"methods",CAPABILITY:"identity",CAPABILITY_JSON:"wire",CapabilityJSON:"go wire",ReadMethod:"go method",Array:[1,2n],Record:{key:"value"}};
const first:MethodOutputs["first/read"]=output;
const second:MethodOutputs["second/read"]=output;
// @ts-expect-error A contract named JSONNumber remains distinct from the imported numeric representation.
const wrong: MethodOutputs["__proto__"] = {JSONNumber:42};
void [input,output,first,second,wrong];
`
	if err := os.WriteFile(filepath.Join(directory, "consumer.ts"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	runTypeScriptConsumer(t, directory)
	command := exec.CommandContext(t.Context(), "go", "test", filepath.Join(directory, "types.go"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated Go namespace: %s\n%v", output, err)
	}
	module := (&url.URL{Scheme: "file", Path: filepath.Join(directory, "methods.generated.ts")}).String()
	runtime := fmt.Sprintf(`import assert from "node:assert/strict";
import {createHash} from "node:crypto";
import {METHODS,CAPABILITY,CAPABILITY_JSON} from %s;
assert.deepEqual(Object.keys(METHODS).sort(), ["__proto__","constructor","toString","first/read","second/read"].sort());
assert.equal(Object.getPrototypeOf(METHODS),Object.prototype);
for (const name of Object.keys(METHODS)) {
  assert.equal(Object.hasOwn(METHODS,name),true);
  assert.equal(METHODS[name].kind,"query");
}
assert.equal(createHash("sha256").update(CAPABILITY_JSON).digest("hex"),CAPABILITY.contract);
for(const method of JSON.parse(CAPABILITY_JSON).methods) {
  assert.equal(method.output.$defs.JSONNumber.type,"string");
  assert.equal(method.output.properties.JSONNumber.$ref,"#/$defs/JSONNumber");
}
`, TypeScriptStringLiteral(module))
	command = exec.CommandContext(t.Context(), "node", "--input-type=module")
	command.Stdin = strings.NewReader(runtime)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated runtime dictionaries: %s\n%v", output, err)
	}
}

func TestTypeScriptNumberImportUsesProjectionSemantics(t *testing.T) {
	directory := t.TempDir()
	generateConsumer(t, directory, []projectionSample{{name: "literal", schema: `{"const":"JSONNumber"}`, wire: `"JSONNumber"`}})
	source, err := os.ReadFile(filepath.Join(directory, "methods.generated.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "import type") {
		t.Fatalf("application string literal created an import:\n%s", source)
	}
}
