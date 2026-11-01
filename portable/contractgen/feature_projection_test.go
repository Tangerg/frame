package contractgen

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/scope/core/jsonschema"
)

func TestStreamingFeatureProjectsChunkAndFinalContracts(t *testing.T) {
	method, err := portable.NewStreamingMethod[struct{}, string, bool](portable.MethodConfig{Name: "model.stream", Kind: portable.Mutation, Target: "call", Description: "Stream a model call."})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := portable.NewCapability("model", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(contract.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := buildFeature(data)
	if err != nil {
		t.Fatal(err)
	}
	if artifacts.methods[0].Stream == "" || !strings.Contains(artifacts.typescript, "export interface MethodChunks") || !strings.Contains(artifacts.typescript, "streaming: true") {
		t.Fatal("stream declaration lost chunk contract")
	}
	generated, err := artifacts.goProjection(featureGoOptions{Package: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), "StreamChunk") {
		t.Fatalf("Go stream chunk projection missing: %s", generated)
	}
}

func TestFeatureCapabilityJSONPreservesIntegerSchemaLexemes(t *testing.T) {
	method, err := portable.NewMethod[struct{}, uint64](portable.MethodConfig{Name: "acme/count", Kind: portable.Query, Target: "installation", Description: "Read the original count."})
	if err != nil {
		t.Fatal(err)
	}
	data, err := featureDescriptor(method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := buildFeature(data)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "export const CAPABILITY_JSON = "
	_, encoded, found := strings.Cut(artifacts.typescript, prefix)
	if !found {
		t.Fatal("canonical capability source omitted")
	}
	encoded, _, _ = strings.Cut(encoded, ";\n")
	original, err := strconv.Unquote(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(original, "18446744073709551615") {
		t.Fatal("uint64 schema bound lost its exact lexeme")
	}
	if original != string(artifacts.capability.Identity().Bytes()) {
		t.Fatal("generated capability differs from its canonical owner encoding")
	}
	contract, err := portable.ParseCapability([]byte(original))
	if err != nil || contract.Identity() != artifacts.capability.Identity() {
		t.Fatalf("generated capability source changed identity: %v", err)
	}
}

type featureStatus string

func (featureStatus) JSONSchemaEnum() []string { return []string{"open", "resolved"} }

type featureInput struct {
	Status   featureStatus `json:"status"`
	Revision int64         `json:"revision"`
}
type featureOutput struct {
	Accepted bool `json:"accepted"`
}

type featurePage[T any] struct {
	Rows []T `json:"rows"`
}

func TestFeatureGenerationPreservesGenericOwnerReferences(t *testing.T) {
	method, err := portable.NewMethod[struct{}, featurePage[featureOutput]](portable.MethodConfig{Name: "acme/read", Kind: portable.Query, Target: "installation", Description: "Read."})
	if err != nil {
		t.Fatal(err)
	}
	data, err := featureDescriptor(method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildFeature(data); err != nil {
		t.Fatalf("generic owner references cannot be generated: %v", err)
	}
}

func TestFeatureGenerationReusesTheAdmittedSchemas(t *testing.T) {
	method, err := portable.NewMethod[featureInput, featureOutput](portable.MethodConfig{Name: "acme/reviews.update", Kind: portable.Mutation, Target: "installation", Description: "Update review."})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := method.Descriptor()
	data, err := featureDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := buildFeature(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated.methods) != 1 || generated.methods[0].Name != descriptor.Name || !strings.Contains(generated.typescript, `"open" | "resolved"`) || !strings.Contains(generated.typescript, "revision: JSONNumber;") || !strings.Contains(generated.typescript, "accepted: boolean;") {
		t.Fatalf("generated surface lost contract: %+v\n%s", generated.methods, generated.typescript)
	}
	contract, err := portable.ParseCapability(data)
	if err != nil || generated.capability.Identity() != contract.Identity() || !strings.Contains(generated.typescript, contract.Identity().Digest().String()) {
		t.Fatalf("generator changed canonical capability identity: %v", err)
	}
	original, err := jsonschema.Parse(descriptor.Input)
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimPrefix(generated.methods[0].Input, "schema.json")
	bundle, err := json.Marshal(struct {
		Schema string                    `json:"$schema"`
		Ref    string                    `json:"$ref"`
		Defs   map[string]jsontext.Value `json:"$defs"`
	}{SchemaDialect, root, generated.definitions})
	if err != nil {
		t.Fatal(err)
	}
	projected, err := jsonschema.Parse(bundle)
	if err != nil {
		t.Fatalf("generated Schema references are broken: %v", err)
	}
	for _, raw := range []string{`{"status":"open","revision":1}`, `{"status":"unknown","revision":1}`, `{"status":"open"}`, `{"status":"open","revision":null}`, `{"status":"open","revision":1,"extra":true}`} {
		if (original.Validate([]byte(raw)) == nil) != (projected.Validate([]byte(raw)) == nil) {
			t.Fatalf("generation changed admission for %s", raw)
		}
	}
}

func TestFeatureGenerationRefusesDuplicatesAndExternalSchemas(t *testing.T) {
	method, err := portable.NewMethod[struct{}, string](portable.MethodConfig{Name: "acme/read", Kind: portable.Query, Target: "installation", Description: "Read."})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := method.Descriptor()
	for _, descriptors := range [][]portable.MethodDescriptor{{descriptor, descriptor}, {{Name: "bad", Kind: portable.Query, Target: "installation", Description: "Read.", Input: []byte(`{"$ref":"https://example.invalid/schema"}`), Output: descriptor.Output}}} {
		data, err := featureDescriptor(descriptors...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildFeature(data); err == nil {
			t.Fatalf("invalid feature contract admitted: %s", data)
		}
	}
	if _, _, err := featureNames([]portable.MethodDescriptor{{Name: "x/read.v1"}, {Name: "y/read-v1"}}); err == nil {
		t.Fatal("two methods were given one generated name")
	}
}

func TestFeatureTypesUseTheExistingEmitterForScalarAndNullableFrames(t *testing.T) {
	for _, raw := range []string{`{"type":"string"}`, `{"oneOf":[{"type":"string"},{"type":"null"}]}`, `{"type":"boolean"}`, `{"type":"object","additionalProperties":{"type":"string"}}`} {
		node, err := featureNode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		emitter := NewTypeScript(Definitions{"Value": node})
		projection := emitter.Definition("Value", node)
		if strings.Contains(projection, "interface") {
			t.Fatalf("scalar/nullable frame was emitted as an object: %s", projection)
		}
	}
}

func TestFeatureTypeProjectionPreservesIntersectionsAndSiblingConstraints(t *testing.T) {
	for _, sample := range []struct{ Schema, want string }{
		{`{"type":"string","allOf":[{"enum":["open","resolved"]}]}`, `string & ("open" | "resolved")`},
		{`{"enum":["open","resolved"],"allOf":[{"const":"resolved"}]}`, `("open" | "resolved") & "resolved"`},
		{`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"allOf":[{"$ref":"#/$defs/Revision"}]}`, `{ id: string } & Revision`},
		{`{"type":"string","allOf":[false]}`, `never`},
		{`{"type":"integer","allOf":[{"minimum":1}]}`, `JSONNumber`},
		{`{"type":"string","allOf":[{"anyOf":[true,{"type":"null"}]}]}`, `string`},
	} {
		t.Run(sample.Schema, func(t *testing.T) {
			node, err := featureNode([]byte(sample.Schema))
			if err != nil {
				t.Fatal(err)
			}
			emitter := NewTypeScript(nil)
			projection := emitter.Definition("Value", node)
			if got := projection; got != "export type Value = "+sample.want+";\n\n" {
				t.Fatalf("type projection = %s, want %s", got, sample.want)
			}
		})
	}
}

func TestFeatureNullableObjectPreservesRequiredProperties(t *testing.T) {
	node, err := featureNode([]byte(`{"type":["object","null"],"properties":{"revision":{"type":"integer"}},"required":["revision"]}`))
	if err != nil {
		t.Fatal(err)
	}
	emitter := NewTypeScript(nil)
	value := emitter.TypeOf(node)
	if !strings.Contains(value, "revision: JSONNumber") || !strings.Contains(value, "null") {
		t.Fatalf("nullable object lost its shape: %s", value)
	}
}

func TestFeatureNumericTypesKeepExactJSONRepresentationAcrossSchemaForms(t *testing.T) {
	for _, kind := range []string{"integer", "number"} {
		for _, sample := range []struct{ schema, want string }{
			{`{"type":"` + kind + `"}`, "JSONNumber"},
			{`{"type":["` + kind + `","null"]}`, "JSONNumber | null"},
			{`{"type":["` + kind + `","string"]}`, "JSONNumber | string"},
			{`{"anyOf":[{"type":"` + kind + `"},{"type":"null"}]}`, "JSONNumber | null"},
			{`{"oneOf":[{"type":"` + kind + `"},{"type":"null"}]}`, "JSONNumber | null"},
		} {
			t.Run(sample.schema, func(t *testing.T) {
				node, err := featureNode([]byte(sample.schema))
				if err != nil {
					t.Fatal(err)
				}
				if got := NewTypeScript(nil).TypeOf(node); got != sample.want {
					t.Fatalf("numeric projection: %s, want %s", got, sample.want)
				}
				descriptor, err := featureDescriptor(portable.MethodDescriptor{Name: "exact.read", Kind: portable.Query, Target: "installation", Description: "Read an exact number.", Input: []byte(`{"type":"object"}`), Output: []byte(sample.schema)})
				if err != nil {
					t.Fatal(err)
				}
				generated, err := buildFeature(descriptor)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(generated.typescript, `import type { JSONNumber } from "@frame/sdk/json";`) {
					t.Fatalf("numeric projection lacks its type import: %s", generated.typescript)
				}
			})
		}
	}
}

func TestFeatureGenerationRefusesFormerBareMethodArray(t *testing.T) {
	if _, err := buildFeature([]byte(`[]`)); err == nil {
		t.Fatal("former generator input was accepted")
	}
}

func TestFeatureGoProjectionPreservesTypedFramesAndIntegerBounds(t *testing.T) {
	type Input struct {
		Limit   *int  `json:"Limit,omitzero"`
		Enabled *bool `json:"Enabled,omitzero"`
	}
	type Output struct {
		Revision uint64
		At       time.Time
		Labels   map[string]string
	}
	method, err := portable.NewMethod[Input, Output](portable.MethodConfig{Name: "acme/read.v1", Kind: portable.Query, Target: "installation", Description: "Read a typed frame."})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := featureDescriptor(method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := buildFeature(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := artifacts.goProjection(featureGoOptions{Package: "consumer", Prefix: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	source := string(generated)
	for _, required := range []string{"Revision uint64", "At", "time.Time", "map[string]string", "*bool", "*int64", "ExampleReadV1Method", "18446744073709551615"} {
		if !strings.Contains(source, required) {
			t.Fatalf("Go projection lost %s", required)
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "generated.go", generated, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		if declared, ok := node.(*ast.ValueSpec); ok && len(declared.Names) == 1 && declared.Names[0].Name == "ExampleCapabilityJSON" {
			encoded, err := strconv.Unquote(declared.Values[0].(*ast.BasicLit).Value)
			if err != nil {
				t.Fatal(err)
			}
			published, err := portable.ParseCapability([]byte(encoded))
			if err != nil || published.Identity() != artifacts.capability.Identity() {
				t.Fatalf("Go projection changed the published contract: %v", err)
			}
			member, _ := published.Method(method.Name())
			if member.Descriptor().Description != method.Descriptor().Description {
				t.Fatal("Go projection discarded published method documentation")
			}
		}
		if declared, ok := node.(*ast.TypeSpec); ok && declared.Assign.IsValid() {
			t.Errorf("consumer boundary reexports an alias: %s", declared.Name.Name)
		}
		return true
	})
	scoped, err := artifacts.goProjection(featureGoOptions{Package: "consumer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"const CapabilityJSON", "const ReadV1Method", "type Input struct", "type Output struct"} {
		if !strings.Contains(string(scoped), required) {
			t.Fatalf("package-scoped projection lost %s", required)
		}
	}
	for _, output := range []featureGoOptions{{Package: "consumer; bad", Prefix: "Example"}, {Package: "consumer", Prefix: "lowercase"}} {
		if _, err := artifacts.goProjection(output); err == nil {
			t.Fatal("invalid Go output configuration accepted")
		}
	}
}

func TestFeatureDefinitionsAreNamedByTheirGoTypes(t *testing.T) {
	schema := func(keys ...string) []byte {
		defs := make([]string, len(keys))
		for index, key := range keys {
			defs[index] = strconv.Quote(key) + `:{"type":"object"}`
		}
		return []byte(`{"$defs":{` + strings.Join(defs, ",") + `}}`)
	}
	_, renamed, err := featureNames([]portable.MethodDescriptor{
		{Name: "feature/read.v1", Input: schema("domain.Snapshot", "model.selection_JSON[string]"), Output: schema("wire.Snapshot")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if renamed["domain.Snapshot"] != "DomainSnapshot" || renamed["wire.Snapshot"] != "WireSnapshot" || renamed["model.selection_JSON[string]"] != "SelectionJSONString" {
		t.Fatalf("definition names = %v", renamed)
	}
}

func TestFeatureDefinitionsRefuseOneNameForTwoBodies(t *testing.T) {
	_, _, err := featureNames([]portable.MethodDescriptor{
		{Name: "feature/read.v1", Input: []byte(`{"$defs":{"domain.Item":{"type":"string"}}}`)},
		{Name: "feature/write.v1", Input: []byte(`{"$defs":{"domain.Item":{"type":"integer"}}}`)},
	})
	if err == nil {
		t.Fatal("two methods published different domain.Item definitions and were accepted")
	}
}
