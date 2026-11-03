package contractgen

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/portable"
)

func TestExpandedRootKeepsAddressedSubschemas(t *testing.T) {
	method := portable.MethodDescriptor{
		Name: "read", Kind: portable.Query, Description: "Read the original field shape.",
		Input: []byte(`{"type":"boolean"}`),
		Output: []byte(`{
			"$defs":{"example.Record":{
				"type":"object",
				"properties":{"value":{"type":"string"},"copy":{"$ref":"#/properties/value"}},
				"required":["value","copy"],"additionalProperties":false
			},"example.Mirror":{"$ref":"#/properties/value"}},
			"type":"object",
			"properties":{"value":{"type":"string"},"copy":{"$ref":"#/properties/value"}},
			"required":["value","copy"],"additionalProperties":false
		}`),
	}
	data, err := featureDescriptor(method)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := portable.ParseCapability(data); err != nil {
		t.Fatalf("original contract: %v", err)
	}
	feature, err := buildFeature(data)
	if err != nil {
		t.Fatalf("root projection lost an addressed subschema: %v", err)
	}
	projectedJSON, err := json.Marshal(struct {
		Ref  string                    `json:"$ref"`
		Defs map[string]jsontext.Value `json:"$defs"`
	}{RefPrefix + "ReadOutput", feature.definitions})
	if err != nil {
		t.Fatal(err)
	}
	projected, err := portable.AdmitSchema(projectedJSON)
	if err != nil {
		t.Fatalf("projected schema lost an addressed subschema: %v", err)
	}
	original, err := portable.AdmitSchema(method.Output)
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []string{`{"copy":"kept","value":"kept"}`, `{"copy":42,"value":"kept"}`, `{"copy":"kept"}`, `null`, `42`, `[]`} {
		if (projected.Validate([]byte(wire)) == nil) != (original.Validate([]byte(wire)) == nil) {
			t.Fatalf("relocating the root changed the predicate for %s", wire)
		}
	}
	goSource, err := feature.goProjection(featureGoOptions{Package: "consumer"})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for name, body := range map[string][]byte{
		"types.go":             goSource,
		"methods.generated.ts": []byte(feature.typescript),
		"consumer_test.go": []byte(`package consumer
import("testing"; json "encoding/json/v2")
func TestRoundTrip(t *testing.T) {
 const wire = "{\"copy\":\"kept\",\"value\":\"kept\"}"
 var decoded Record
 if err:=json.Unmarshal([]byte(wire), &decoded);err!=nil{t.Fatal(err)}
 got,err:=json.Marshal(decoded,json.Deterministic(true))
 if err!=nil||string(got)!=wire{t.Fatalf("original fields changed: %s, %v",got,err)}
}
`),
		"consumer.ts": []byte(`import type {ReadOutput,Mirror} from "./methods.generated.js";
const original: ReadOutput = {value:"kept",copy:"kept"};
const mirrored: Mirror = "kept";
// @ts-expect-error The copied field still has its original string shape.
const invalid: ReadOutput = {value:"kept",copy:42};
void [original,mirrored,invalid];
`),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(t.Context(), "go", "test", filepath.Join(directory, "types.go"), filepath.Join(directory, "consumer_test.go"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated Go root consumer: %s\n%v", output, err)
	}
	runTypeScriptConsumer(t, directory)
}
