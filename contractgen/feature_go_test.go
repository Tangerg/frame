package contractgen

import (
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"testing"

	"github.com/Tangerg/frame"
)

func TestGoStringEnumsOnlyPublishAdmittedStringLiterals(t *testing.T) {
	for _, test := range []struct {
		name   string
		schema string
		values []string
	}{
		{name: "mixed literals", schema: `{"type":"string","enum":[null,true,42,{},[],"approved"]}`, values: []string{"approved"}},
		{name: "empty string", schema: `{"type":"string","enum":[null,"","approved"]}`, values: []string{"", "approved"}},
		{name: "composed enum", schema: `{"type":"string","allOf":[{"enum":[null,"approved"]}]}`, values: []string{"approved"}},
		{name: "pattern narrows enum", schema: `{"type":"string","enum":["approved","denied"],"pattern":"^approved$"}`, values: []string{"approved"}},
		{name: "union narrows enum", schema: `{"type":"string","enum":["approved","denied"],"anyOf":[{"const":"approved"},{"const":"pending"}]}`, values: []string{"approved"}},
		{name: "exclusive union narrows enum", schema: `{"type":"string","enum":["approved","denied"],"oneOf":[{"enum":["approved","denied"]},{"const":"denied"}]}`, values: []string{"approved"}},
		{name: "reference narrows enum", schema: `{"$defs":{"Decision":{"type":"string","enum":["approved"]}},"type":"string","enum":["approved","denied"],"$ref":"#/$defs/Decision"}`, values: []string{"approved"}},
		{name: "negation narrows enum", schema: `{"type":"string","enum":["approved","denied"],"not":{"const":"denied"}}`, values: []string{"approved"}},
		{name: "conditional narrows enum", schema: `{"type":"string","enum":["approved","denied"],"if":{"const":"denied"},"then":false}`, values: []string{"approved"}},
		{name: "format narrows enum", schema: `{"type":"string","enum":["2026-11-12","invalid"],"format":"date"}`, values: []string{"2026-11-12"}},
		{name: "null constant", schema: `{"type":"string","const":null}`},
		{name: "string constant", schema: `{"type":"string","const":"approved"}`, values: []string{"approved"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor := frame.MethodDescriptor{
				Name: "acme/read", Kind: frame.Query, Target: "installation", Description: "Read a review.",
				Input: []byte(`{"type":"boolean"}`), Output: []byte(test.schema),
			}
			method, err := frame.AdmitMethod(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			data, err := featureDescriptor(method.Descriptor())
			if err != nil {
				t.Fatal(err)
			}
			feature, err := buildFeature(data)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := feature.goProjection(featureGoOptions{Package: "consumer"})
			if err != nil {
				t.Fatal(err)
			}
			files := token.NewFileSet()
			file, err := parser.ParseFile(files, "contract.go", projection, parser.AllErrors)
			if err != nil {
				t.Fatal(err)
			}
			compiled, err := (&types.Config{}).Check("consumer", files, []*ast.File{file}, nil)
			if err != nil {
				t.Fatalf("generated enum does not compile: %v\n%s", err, projection)
			}
			output := compiled.Scope().Lookup("ReadOutput")
			if output == nil {
				t.Fatal("generated contract omitted its output type")
			}
			var values []string
			for _, name := range compiled.Scope().Names() {
				member, ok := compiled.Scope().Lookup(name).(*types.Const)
				if !ok || !types.Identical(member.Type(), output.Type()) {
					continue
				}
				value := constant.StringVal(member.Val())
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := method.ValidateOutput(encoded); err != nil {
					t.Fatalf("generated member %s=%q violates its original contract: %v", name, value, err)
				}
				values = append(values, value)
			}
			slices.Sort(values)
			if !slices.Equal(values, test.values) {
				t.Fatalf("published values=%q, want %q", values, test.values)
			}
		})
	}
}

func TestGoProjectionPreservesPublishedStringEnumOwnership(t *testing.T) {
	method := frame.MethodDescriptor{
		Name: "acme/review.v2", Kind: frame.Command, Target: "installation", Description: "Review a record.",
		Input:  []byte(`{"type":"object","properties":{"decision":{"type":"string","allOf":[{"enum":["approve","reject"]}]}},"required":["decision"],"additionalProperties":false}`),
		Output: []byte(`{"type":"boolean"}`),
	}
	data, err := featureDescriptor(method)
	if err != nil {
		t.Fatal(err)
	}
	feature, err := buildFeature(data)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := feature.goProjection(featureGoOptions{Package: "consumer"})
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "contract.go", projection, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	config := types.Config{}
	compiled, err := config.Check("consumer", files, []*ast.File{file}, nil)
	if err != nil {
		t.Fatalf("generated contract does not compile: %v\n%s", err, projection)
	}
	decision := compiled.Scope().Lookup("ReviewV2InputDecision")
	input := compiled.Scope().Lookup("ReviewV2Input").Type().Underlying().(*types.Struct)
	if decision == nil || !types.Identical(input.Field(0).Type(), decision.Type()) {
		t.Fatal("input field lost its published enum type")
	}
	for name, value := range map[string]string{"Approve": "approve", "Reject": "reject"} {
		member, ok := compiled.Scope().Lookup("ReviewV2InputDecision" + name).(*types.Const)
		if !ok || !types.Identical(member.Type(), decision.Type()) || constant.StringVal(member.Val()) != value {
			t.Fatalf("published enum member %s changed", name)
		}
	}
}

func TestGoProjectionPreservesCollidingEnumValues(t *testing.T) {
	method := frame.MethodDescriptor{
		Name: "acme/review", Kind: frame.Command, Target: "installation", Description: "Review a record.",
		Input:  []byte(`{"type":"object","properties":{"decision":{"type":"string","enum":["a-b","a_b"]}},"required":["decision"],"additionalProperties":false}`),
		Output: []byte(`{"type":"boolean"}`),
	}
	data, err := featureDescriptor(method)
	if err != nil {
		t.Fatal(err)
	}
	feature, err := buildFeature(data)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := feature.goProjection(featureGoOptions{Package: "consumer"})
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "contract.go", projection, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := (&types.Config{}).Check("consumer", files, []*ast.File{file}, nil)
	if err != nil {
		t.Fatalf("generated enum does not compile: %v\n%s", err, projection)
	}
	values := make(map[string]bool)
	for _, name := range compiled.Scope().Names() {
		member, ok := compiled.Scope().Lookup(name).(*types.Const)
		if ok && types.Identical(member.Type(), compiled.Scope().Lookup("ReviewInputDecision").Type()) {
			values[constant.StringVal(member.Val())] = true
		}
	}
	if len(values) != 2 || !values["a-b"] || !values["a_b"] {
		t.Fatalf("published enum values changed: %v", values)
	}
}
