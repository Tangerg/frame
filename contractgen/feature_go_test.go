package contractgen

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/Tangerg/frame/portable"
)

func TestGoProjectionPreservesPublishedStringEnumOwnership(t *testing.T) {
	method := portable.MethodDescriptor{
		Name: "acme/review.v2", Kind: portable.Mutation, Target: "installation", Description: "Review a record.",
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

func TestGoProjectionRejectsCollidingEnumIdentifiers(t *testing.T) {
	method := portable.MethodDescriptor{
		Name: "acme/review", Kind: portable.Mutation, Target: "installation", Description: "Review a record.",
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
	if _, err := feature.goProjection(featureGoOptions{Package: "consumer"}); err == nil {
		t.Fatal("two published values received one Go identifier")
	}
}
