package portable_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Tangerg/frame/portable"
	framevalue "github.com/Tangerg/frame/value"
	"github.com/Tangerg/scope/core/jsonschema"
)

func TestPortableDescriptorLimitsPrecedeCompilationAndCannotBeReconfigured(t *testing.T) {
	limits := portable.ProfileLimits()
	copy := limits
	copy.MaxDescriptorBytes = 1
	if portable.ProfileLimits() != limits {
		t.Fatal("profile projection changed the owner")
	}
	for _, test := range []struct {
		name string
		raw  string
	}{
		{"bytes", `{"description":"` + strings.Repeat("a", limits.MaxDescriptorBytes-len(`{"description":""}`)) + `"}`},
		{"nodes", `[` + strings.Repeat("0,", limits.MaxDescriptorNodes-2) + `0]`},
		{"depth", strings.Repeat("[", limits.MaxJSONDepth-1) + `0` + strings.Repeat("]", limits.MaxJSONDepth-1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := portable.CheckDescriptor([]byte(test.raw)); err != nil {
				t.Fatalf("exact boundary: %v", err)
			}
			var tooLarge string
			switch test.name {
			case "bytes":
				tooLarge = test.raw + " "
			case "nodes":
				tooLarge = test.raw[:len(test.raw)-1] + ",0]"
			case "depth":
				tooLarge = "[" + test.raw + "]"
			}
			if err := portable.CheckDescriptor([]byte(tooLarge)); !errors.Is(err, portable.ErrLimit) {
				t.Fatalf("over boundary: %v", err)
			}
		})
	}
	methods := make([]portable.MethodDescriptor, limits.MaxMethods+1)
	for index := range methods {
		methods[index] = testMethod("limit/read/"+strconv.Itoa(index), portable.Query)
	}
	config := "limits"
	if _, err := portable.NewCapability(config, methods[:limits.MaxMethods]...); err != nil {
		t.Fatalf("exact method capacity: %v", err)
	}
	if _, err := portable.NewCapability(config, methods...); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("over method capacity: %v", err)
	}
	method := testMethod("limit/read", portable.Query)
	method.Description = strings.Repeat("x", limits.MaxDescriptorBytes)
	if _, err := portable.AdmitMethod(method); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("typed constructor bypassed total descriptor budget: %v", err)
	}
}

func referenceChain(depth int, duplicate bool) []byte {
	defs := map[string]any{"d0": true}
	for index := 1; index < depth; index++ {
		ref := map[string]any{"$ref": "#/$defs/d" + strconv.Itoa(index-1)}
		if duplicate {
			defs["d"+strconv.Itoa(index)] = map[string]any{"allOf": []any{ref, ref}}
		} else {
			defs["d"+strconv.Itoa(index)] = ref
		}
	}
	raw, err := json.Marshal(map[string]any{"$defs": defs, "$ref": "#/$defs/d" + strconv.Itoa(depth-1)})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestPortableSchemaReferencesHaveFiniteAdmissionWork(t *testing.T) {
	for _, raw := range []string{
		`{"$ref":"#"}`,
		`{"anyOf":[true,{"$ref":"#"}]}`,
		`{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"allOf":[{"$ref":"#/$defs/a"}]}},"$ref":"#/$defs/a"}`,
		`{"$ref":"https://example.invalid/schema"}`,
		`{"$id":"https://example.invalid/local","type":"string"}`,
	} {
		if _, err := portable.AdmitSchema([]byte(raw)); err == nil {
			t.Fatalf("unsupported reference graph admitted: %s", raw)
		}
	}
	limits := portable.ProfileLimits()
	if _, err := portable.AdmitSchema(referenceChain(limits.MaxReferenceDepth-1, false)); err != nil {
		t.Fatalf("exact reference depth: %v", err)
	}
	if _, err := portable.AdmitSchema(referenceChain(limits.MaxReferenceDepth, false)); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("deep reference chain: %v", err)
	}
	if _, err := portable.AdmitSchema(referenceChain(14, true)); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("exponential reference graph: %v", err)
	}
	refs := make([]any, limits.MaxReferences+1)
	for index := range refs {
		refs[index] = map[string]any{"$ref": "#/$defs/value"}
	}
	raw, err := json.Marshal(map[string]any{"$defs": map[string]any{"value": true}, "allOf": refs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := portable.AdmitSchema(raw); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("reference count: %v", err)
	}
}

func nestedObject(depth int) []byte {
	return []byte(strings.Repeat(`{"next":`, depth-1) + `{}` + strings.Repeat("}", depth-1))
}

func TestPortableRecursiveSchemaBoundsQueryOutputAndStreamBeforeValidation(t *testing.T) {
	shape := jsontext.Value(`{"type":"object","properties":{"next":{"$ref":"#"}},"additionalProperties":false}`)
	method, err := portable.AdmitMethod(portable.MethodDescriptor{Name: "tree/read", Kind: portable.Query, Target: "workspace", Input: shape, Output: shape, Stream: shape})
	if err != nil {
		t.Fatal(err)
	}
	for _, depth := range []int{3, portable.ProfileLimits().MaxJSONDepth, portable.ProfileLimits().MaxJSONDepth + 1} {
		raw := nestedObject(depth)
		input, err := framevalue.ParseJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		for name, err := range map[string]error{"input": method.ValidateInput(input), "output": method.ValidateOutput(raw), "chunk": method.ValidateChunk(raw)} {
			if depth <= portable.ProfileLimits().MaxJSONDepth && err != nil {
				t.Fatalf("%s depth %d: %v", name, depth, err)
			}
			if depth > portable.ProfileLimits().MaxJSONDepth && !errors.Is(err, portable.ErrLimit) {
				t.Fatalf("%s excessive depth: %v", name, err)
			}
		}
	}
	branching, err := portable.AdmitSchema([]byte(`{"type":"object","allOf":[{"properties":{"next":{"$ref":"#"}}},{"properties":{"next":{"$ref":"#"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := branching.Validate(nestedObject(4)); err != nil {
		t.Fatalf("bounded structural recursion: %v", err)
	}
	if err := branching.Validate(nestedObject(40)); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("recursive evaluation work: %v", err)
	}
}

func TestPortableNumericLimitsKeepWideEvidenceWithoutExpandingHostileExponents(t *testing.T) {
	limits := portable.ProfileLimits()
	numbers := []string{"-9223372036854775808", "18446744073709551615", "1.2300e-10", "1e" + strconv.Itoa(limits.MaxAbsExponent), strings.Repeat("9", limits.MaxNumberBytes)}
	method := testMethod("number/read", portable.Query)
	method.Output, method.Stream = []byte(`{"type":"number"}`), []byte(`{"type":"number"}`)
	contract, err := portable.AdmitMethod(method)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range numbers {
		if err := contract.ValidateOutput([]byte(raw)); err != nil {
			t.Fatalf("normal exact number %s: %v", raw[:min(len(raw), 32)], err)
		}
		if err := contract.ValidateChunk([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"1e1000000000", "1e-1000000000", "1e" + strconv.Itoa(limits.MaxAbsExponent+1), strings.Repeat("9", limits.MaxNumberBytes+1)} {
		input, err := framevalue.ParseJSON([]byte(`{"revision":` + raw + `}`))
		if err != nil {
			t.Fatal(err)
		}
		for name, err := range map[string]error{"input": contract.ValidateInput(input), "output": contract.ValidateOutput([]byte(raw)), "stream": contract.ValidateChunk([]byte(raw))} {
			if !errors.Is(err, portable.ErrLimit) {
				t.Fatalf("%s hostile number: %v", name, err)
			}
		}
		for _, keyword := range []string{"minimum", "maximum", "const"} {
			if _, err := portable.AdmitSchema(fmt.Appendf(nil, `{"%s":%s}`, keyword, raw)); !errors.Is(err, portable.ErrLimit) {
				t.Fatalf("schema %s hostile number: %v", keyword, err)
			}
		}
		if _, err := portable.AdmitSchema([]byte(`{"enum":[` + raw + `]}`)); !errors.Is(err, portable.ErrLimit) {
			t.Fatalf("schema enum hostile number: %v", err)
		}
	}
}

func TestPortableValueBudgetsPreserveLargeConsumerPayloads(t *testing.T) {
	// This is the actual public embedding projection: a space and a batch of vectors.
	type embeddings struct {
		Space   string      `json:"space"`
		Vectors [][]float32 `json:"vectors"`
	}
	derived, err := jsonschema.For[embeddings]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := portable.AdmitSchema(derived.JSON())
	if err != nil {
		t.Fatal(err)
	}
	output := embeddings{Space: "captured-model", Vectors: make([][]float32, 64)}
	for index := range output.Vectors {
		output.Vectors[index] = make([]float32, 3072)
	}
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(raw); err != nil {
		t.Fatalf("64 x 3072 vector response: %v", err)
	}
	chat, err := portable.AdmitSchema([]byte(`{"type":"object","properties":{"messages":{"type":"array","items":{"type":"object","properties":{"role":{"enum":["user","assistant"]},"content":{"type":"string"}}}}},"required":["messages"]}`))
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]map[string]string, 128)
	for index := range messages {
		messages[index] = map[string]string{"role": "user", "content": strings.Repeat("x", 16*1024)}
	}
	raw, err = json.Marshal(map[string]any{"messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= portable.ProfileLimits().MaxDescriptorBytes {
		t.Fatal("fixture must exercise payload bytes beyond the descriptor limit")
	}
	if err := chat.Validate(raw); err != nil {
		t.Fatalf("large caller-budgeted chat request: %v", err)
	}
}

func TestPortablePatternsHaveNoAuthorControlledBacktracking(t *testing.T) {
	for _, pattern := range []string{`\S`, `^[0-9a-fA-F]{40}$`, `^mem_[0-9a-f]{32}$`} {
		raw, _ := json.Marshal(map[string]string{"type": "string", "pattern": pattern})
		if _, err := portable.AdmitSchema(raw); err != nil {
			t.Fatalf("fixed pattern %q: %v", pattern, err)
		}
	}
	for _, pattern := range []string{`(a+)+$`, `a|aa`, `a*`, `a{1,100}`, `(a)\1`, `a{1000000000}`} {
		raw, _ := json.Marshal(map[string]string{"type": "string", "pattern": pattern})
		if _, err := portable.AdmitSchema(raw); err == nil {
			t.Fatalf("unbounded pattern admitted: %q", pattern)
		}
	}
}

func TestPortableEqualityWorkIncludesTextAndOnlyChargesRelevantSchemas(t *testing.T) {
	large := strings.Repeat("x", portable.ProfileLimits().MaxEqualityWork/8)
	values := []any{"a" + large, "b" + large, "c" + large, "d" + large}
	raw, err := json.Marshal(map[string]any{"enum": values})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= portable.ProfileLimits().MaxDescriptorBytes {
		t.Fatal("enum fixture must fit the descriptor byte budget")
	}
	if _, err := portable.AdmitSchema(raw); !errors.Is(err, portable.ErrLimit) || !strings.Contains(err.Error(), "equality work") {
		t.Fatalf("enum text comparison budget: %v", err)
	}
	unique, err := portable.AdmitSchema([]byte(`{"type":"array","uniqueItems":true}`))
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := portable.AdmitSchema([]byte(`{"type":"array"}`))
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]any, len(values))
	for index := range keys {
		keys[index] = map[string]int{strconv.Itoa(index) + large: index}
	}
	numbers := []byte("[" + strings.Repeat(strings.Repeat("9", 512)+",", 63) + strings.Repeat("8", 512) + "]")
	for name, value := range map[string]any{"strings": values, "keys": keys} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := ordinary.Validate(raw); err != nil {
			t.Fatalf("ordinary %s payload: %v", name, err)
		}
		if err := unique.Validate(raw); !errors.Is(err, portable.ErrLimit) || !strings.Contains(err.Error(), "equality work") {
			t.Fatalf("unique %s comparison budget: %v", name, err)
		}
	}
	if err := ordinary.Validate(numbers); err != nil {
		t.Fatal(err)
	}
	if err := unique.Validate(numbers); !errors.Is(err, portable.ErrLimit) || !strings.Contains(err.Error(), "equality work") {
		t.Fatalf("number literal comparison budget: %v", err)
	}
	if err := unique.Validate([]byte(`[1,2,3]`)); err != nil {
		t.Fatalf("normal uniqueness predicate: %v", err)
	}
	if err := unique.Validate([]byte(`[1,1]`)); err == nil || errors.Is(err, portable.ErrLimit) {
		t.Fatalf("duplicate verdict must remain the schema engine's: %v", err)
	}
}

func TestSchemaSizeConstraintsUseExactNonnegativeIntegers(t *testing.T) {
	data, err := os.ReadFile("testdata/schema_sizes.json")
	if err != nil {
		t.Fatal(err)
	}
	var samples []struct {
		Literal  string `json:"literal"`
		Admitted bool   `json:"admitted"`
	}
	if err := json.Unmarshal(data, &samples); err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		for _, keyword := range []string{"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties"} {
			_, err := portable.AdmitSchema([]byte(fmt.Sprintf("{%q:%s}", keyword, sample.Literal)))
			if (err == nil) != sample.Admitted {
				t.Errorf("%s %s admission: %v", keyword, sample.Literal, err)
			}
		}
	}
}
