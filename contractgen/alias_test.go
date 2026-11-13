package contractgen

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

func TestGeneratedAliasChainsHaveOneDeterministicOwner(t *testing.T) {
	method := frame.MethodDescriptor{
		Name: "read", Kind: frame.Query, Description: "Read an aliased value.",
		Input: []byte(`{"type":"boolean"}`),
		Output: []byte(`{
			"$defs":{
				"example.Public":{"$ref":"#/$defs/example.first"},
				"example.first":{"$ref":"#/$defs/example.second"},
				"example.second":{"$ref":"#/$defs/example.third"},
				"example.third":{"$ref":"#/$defs/example.text"},
				"example.text":{"type":"string"}
			},
			"type":"object",
			"properties":{
				"head":{"$ref":"#/$defs/example.Public"},
				"middle":{"$ref":"#/$defs/example.second"},
				"tail":{"$ref":"#/$defs/example.text"}
			},
			"required":["head","middle","tail"],
			"additionalProperties":false
		}`),
	}
	data, err := featureDescriptor(method)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := frame.ParseCapability(data)
	if err != nil {
		t.Fatal(err)
	}
	var typescript string
	var goSource, schema []byte
	for iteration := range 24 {
		feature, err := buildFeature(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(feature.definitions) != 3 || string(feature.definitions["Public"]) != `{"type":"string"}` {
			t.Fatalf("aliases acquired competing representations: %v", feature.definitions)
		}
		if feature.capability.Identity() != contract.Identity() {
			t.Fatal("projection changed the published capability identity")
		}
		generatedGo, err := feature.goProjection(featureGoOptions{Package: "consumer"})
		if err != nil {
			t.Fatal(err)
		}
		generatedSchema, err := feature.schemaJSON()
		if err != nil {
			t.Fatal(err)
		}
		if iteration == 0 {
			typescript, goSource, schema = feature.typescript, generatedGo, generatedSchema
			continue
		}
		if typescript != feature.typescript || !bytes.Equal(goSource, generatedGo) || !bytes.Equal(schema, generatedSchema) {
			t.Fatal("one descriptor generated different projection namespaces")
		}
	}
	directory := t.TempDir()
	for name, data := range map[string][]byte{
		"types.go":             goSource,
		"methods.generated.ts": []byte(typescript),
		"consumer_test.go": []byte(`package consumer
const text Public = "kept"
var _ = ReadOutput{Head:text, Middle:text, Tail:text}
`),
		"consumer.ts": []byte(`import type {Public,ReadOutput} from "./methods.generated.js";
const text: Public = "kept";
const output: ReadOutput = {head:text,middle:text,tail:text};
// @ts-expect-error Every alias retains the canonical string shape.
const wrong: ReadOutput["middle"] = 42;
void [output,wrong];
`),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(t.Context(), "go", "test", filepath.Join(directory, "types.go"), filepath.Join(directory, "consumer_test.go"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated Go alias consumer: %s\n%v", output, err)
	}
	runTypeScriptConsumer(t, directory)
}

func TestGeneratedReferenceSiblingsRetainTheirSchema(t *testing.T) {
	for _, sample := range []struct {
		name, sibling, value string
	}{
		{"constraint", "maxLength", `2`},
		{"annotation", "description", `"The public declaration owns this description."`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			alias, err := json.Marshal(map[string]jsontext.Value{"$ref": []byte(`"#/$defs/example.b"`), sample.sibling: []byte(sample.value)})
			if err != nil {
				t.Fatal(err)
			}
			original, err := json.Marshal(map[string]any{
				"$defs": map[string]jsontext.Value{"example.A": alias, "example.b": []byte(`{"type":"string"}`)},
				"$ref":  "#/$defs/example.A",
			})
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			generateConsumer(t, directory, []projectionSample{{name: "read", schema: string(original), wire: `"ok"`}})
			bundle, err := os.ReadFile(filepath.Join(directory, "schema", "schema.json"))
			if err != nil {
				t.Fatal(err)
			}
			var projected struct {
				Defs map[string]jsontext.Value `json:"$defs"`
			}
			if err := json.Unmarshal(bundle, &projected); err != nil {
				t.Fatal(err)
			}
			var retained map[string]jsontext.Value
			if err := json.Unmarshal(projected.Defs["A"], &retained); err != nil {
				t.Fatal(err)
			}
			var target struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(projected.Defs["B"], &target); err != nil {
				t.Fatal(err)
			}
			if string(retained["$ref"]) != `"#/$defs/B"` || string(retained[sample.sibling]) != sample.value || target.Type != "string" {
				t.Fatalf("reference sibling lost its independent schema: %s", bundle)
			}
			root, err := json.Marshal(map[string]any{"$defs": projected.Defs, "$ref": RefPrefix + "ReadOutput"})
			if err != nil {
				t.Fatal(err)
			}
			before, err := framevalue.AdmitSchema(original)
			if err != nil {
				t.Fatal(err)
			}
			after, err := framevalue.AdmitSchema(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, wire := range []string{`""`, `"ok"`, `"long"`, `42`, `false`, `null`} {
				if (before.Validate([]byte(wire)) == nil) != (after.Validate([]byte(wire)) == nil) {
					t.Fatalf("reference sibling changed admission for %s", wire)
				}
			}
			for name, source := range map[string]string{
				"consumer_test.go": "package consumer\nvar _ B = \"ok\"\n",
				"consumer.ts": `import type {ReadOutput} from "./methods.generated.js";
const value: ReadOutput = "ok";
// @ts-expect-error The reference still requires a string.
const wrong: ReadOutput = 42;
void [value,wrong];
`,
			} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.CommandContext(t.Context(), "go", "test", filepath.Join(directory, "types.go"), filepath.Join(directory, "consumer_test.go"))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("generated Go reference sibling consumer: %s\n%v", output, err)
			}
			runTypeScriptConsumer(t, directory)
		})
	}
}
