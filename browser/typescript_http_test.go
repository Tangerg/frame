package browser

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Tangerg/frame/value"
)

type httpProjectionPhase string

func (httpProjectionPhase) JSONSchemaEnum() []string { return []string{"draft", "ready"} }

type httpProjectionQuery struct {
	ID string `json:"id"`
}

type httpProjectionCreate struct {
	Title string `json:"title"`
}

type httpProjectionSummary struct {
	Count int64 `json:"count"`
}

type httpInputs struct{}

type wireObject struct{}

type httpProjectionValue struct {
	Phase    httpProjectionPhase `json:"phase"`
	Small    int32               `json:"small"`
	Signed   int64               `json:"signed"`
	Unsigned uint64              `json:"unsigned"`
	Title    *string             `json:"title"`
	Note     *string             `json:"note,omitempty"`
	Code     modeledWireCode     `json:"code"`
	Number   value.Number        `json:"number"`
	Payload  nullableWireJSON    `json:"payload"`
	Pair     [2]string           `json:"pair"`
}

func TestGeneratedHTTPConsumerPreservesOperationAndWireContracts(t *testing.T) {
	operations := []HTTPOperation{
		{Name: "lookup", Method: http.MethodGet, Path: "/api/items", SuccessStatus: http.StatusOK, Input: reflect.TypeFor[httpProjectionQuery](), Output: reflect.TypeFor[httpProjectionValue]()},
		{Name: "create", Method: http.MethodPost, Path: "/api/items", SuccessStatus: http.StatusOK, Input: reflect.TypeFor[httpProjectionCreate](), Output: reflect.TypeFor[httpProjectionValue]()},
		{Name: "summary", Method: http.MethodGet, Path: "/api/summary", SuccessStatus: http.StatusOK, Output: reflect.TypeFor[httpProjectionSummary]()},
		{Name: "close", Method: http.MethodPost, Path: "/api/close", SuccessStatus: http.StatusNoContent},
		{Name: "opaque", Method: http.MethodGet, Path: "/api/opaque", SuccessStatus: http.StatusOK, Output: reflect.TypeFor[nullableWireJSON]()},
		{Name: "__proto__", Method: http.MethodGet, Path: "/api/prototype-name", SuccessStatus: http.StatusOK, Output: reflect.TypeFor[httpProjectionSummary]()},
	}
	var out strings.Builder
	if err := WriteTypeScriptHTTP(&out, operations, reflect.TypeFor[httpInputs](), reflect.TypeFor[wireObject]()); err != nil {
		t.Fatal(err)
	}
	checkTypeScriptConsumer(t, out.String(), `
import { decodeHTTPOutput, decodeWireValue, httpOperations, httpRequestPath, type HTTPOutputs, type httpProjectionValue } from "./types.generated.js";
import { ExactNumber } from "@frame/sdk/json";
const value: httpProjectionValue = {phase: "ready", small: 42, signed: -9223372036854775808n, unsigned: 18446744073709551615n, title: "title", code: "encoded", number: new ExactNumber("1e400"), payload: null, pair: ["left", "right"]};
const method: "GET" = httpOperations.lookup.method;
const lookup: HTTPOutputs["lookup"] = decodeHTTPOutput("lookup", value);
const summary: HTTPOutputs["summary"] = decodeHTTPOutput("summary", {count: 1});
const acknowledged: void = decodeHTTPOutput("close", null);
const checked: httpProjectionValue = decodeWireValue("httpProjectionValue", value);
httpRequestPath("lookup", {id: "item"});
httpRequestPath("create", {title: "title"});
httpRequestPath("summary", undefined);
// @ts-expect-error The endpoint alone chooses its input; inference cannot widen to another operation.
httpRequestPath("lookup", {title: "title"});
// @ts-expect-error A body for another operation cannot select a different endpoint implicitly.
httpRequestPath("create", {id: "item"});
// @ts-expect-error An operation with no request body does not accept an arbitrary object.
httpRequestPath("summary", {});
// @ts-expect-error The operation owns its result type; a consumer cannot claim another response.
const wrongResult: HTTPOutputs["summary"] = decodeHTTPOutput("lookup", value);
// @ts-expect-error Enum membership comes from the Go source, not its underlying string model.
const unknownPhase: httpProjectionValue = {...value, phase: "unknown"};
// @ts-expect-error A private codec representation is not the declared JSON value.
const privateCode: httpProjectionValue = {...value, code: {privateValue: "encoded"}};
// @ts-expect-error A required pointer is present and non-null on the wire.
const nullTitle: httpProjectionValue = {...value, title: null};
void [method, lookup, summary, acknowledged, checked, wrongResult, unknownPhase, privateCode, nullTitle];
`)
	const consumer = `
import assert from "node:assert/strict";
import { ExactNumber, parseJSON } from "@frame/sdk/json";
import { decodeHTTPOutput, decodeWireValue, httpRequestPath } from "./types.generated.ts";

const valid = parseJSON('{"phase":"ready","small":2147483647,"signed":-9223372036854775808,"unsigned":18446744073709551615,"title":"title","code":"encoded","number":1e400,"payload":null,"pair":["left","right"]}');
assert.deepEqual(decodeHTTPOutput("lookup", valid), valid);
assert.deepEqual(decodeWireValue("httpProjectionValue", valid), valid);
assert.equal(decodeHTTPOutput("opaque", null), null);
assert.equal(decodeHTTPOutput("close", null), undefined);
assert.deepEqual(decodeHTTPOutput("__proto__", {count: 1}), {count: 1});
assert.deepEqual(decodeHTTPOutput("lookup", {...valid, signed: new ExactNumber("42.0")}), {...valid, signed: new ExactNumber("42.0")});

const missingTitle = {...valid};
delete missingTitle.title;
for (const invalid of [
  null, {}, {...valid, phase: "unknown"}, {...valid, extra: true}, missingTitle,
  {...valid, title: null}, {...valid, note: null}, {...valid, note: undefined},
  {...valid, small: 2147483648}, {...valid, small: -2147483649}, {...valid, small: 0.5},
  {...valid, signed: -9223372036854775809n}, {...valid, signed: 9223372036854775808n},
  {...valid, signed: new ExactNumber("0.1")}, {...valid, unsigned: -1n},
  {...valid, unsigned: 18446744073709551616n}, {...valid, number: "1e400"},
  {...valid, number: Number.POSITIVE_INFINITY}, {...valid, code: {privateValue: "encoded"}},
  {...valid, pair: ["left"]}, {...valid, pair: ["left", "right", "extra"]},
]) assert.throws(() => decodeHTTPOutput("lookup", invalid), TypeError);
assert.throws(() => decodeHTTPOutput("close", {}), TypeError);
assert.throws(() => decodeHTTPOutput("lookup", {count: 1}), TypeError);
assert.throws(() => decodeHTTPOutput("toString", valid), TypeError);
assert.throws(() => decodeWireValue("toString", valid), TypeError);
assert.throws(() => httpRequestPath("lookup", {}), TypeError);
assert.throws(() => httpRequestPath("lookup", {id: 1}), TypeError);
assert.throws(() => httpRequestPath("lookup", {id: "item", extra: true}), TypeError);
assert.throws(() => httpRequestPath("create", {id: "item"}), TypeError);
assert.throws(() => httpRequestPath("summary", {}), TypeError);
assert.throws(() => httpRequestPath("toString", undefined), TypeError);
assert.equal(httpRequestPath("create", {title: "title"}), "/api/items");
assert.equal(httpRequestPath("summary", undefined), "/api/summary");
for (const id of ["item", ".", "..", "", "slash?/%", "空 格", "a+b&x=y#fragment"]) {
  const url = new URL(httpRequestPath("lookup", {id}), "https://example.invalid");
  assert.equal(url.origin, "https://example.invalid");
  assert.equal(url.pathname, "/api/items");
  assert.equal(url.hash, "");
  assert.deepEqual([...url.searchParams], [["id", id]]);
}
`
	const loader = `
import { registerHooks } from "node:module";
import { pathToFileURL } from "node:url";

const jsonModule = pathToFileURL(process.argv[2]).href;
registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier === "@frame/sdk/json") return {url: jsonModule, shortCircuit: true};
    return nextResolve(specifier, context);
  },
});
await import("./consumer.mjs");
`
	directory := t.TempDir()
	for _, file := range []struct{ name, source string }{
		{name: "types.generated.ts", source: out.String()},
		{name: "consumer.mjs", source: consumer},
		{name: "loader.mjs", source: loader},
	} {
		if err := os.WriteFile(filepath.Join(directory, file.name), []byte(file.source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	module, err := filepath.Abs("../sdk/src/json.ts")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "node", filepath.Join(directory, "loader.mjs"), module)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated HTTP boundary changed its source contract: %s\n%v", output, err)
	}
}

type httpOptionalQuery struct {
	ID string `json:"id,omitempty"`
}

type httpNumericQuery struct {
	ID int32 `json:"id"`
}

type HTTPInputs struct{}

type wire string

func (wire) JSONSchemaEnum() []string { return []string{"value"} }

type matchesWire string

func (matchesWire) JSONSchemaEnum() []string { return []string{"value"} }

type Values struct{}

func TestTypeScriptHTTPRejectsInvalidContractsBeforePublication(t *testing.T) {
	valid := HTTPOperation{Name: "lookup", Method: http.MethodGet, Path: "/api/items", SuccessStatus: http.StatusOK, Input: reflect.TypeFor[httpProjectionQuery](), Output: reflect.TypeFor[httpProjectionSummary]()}
	for _, test := range []struct {
		name       string
		operations []HTTPOperation
		roots      []reflect.Type
	}{
		{name: "empty operations"},
		{name: "duplicate operation", operations: []HTTPOperation{valid, valid}},
		{name: "duplicate binding", operations: []HTTPOperation{valid, {Name: "other", Method: valid.Method, Path: valid.Path, SuccessStatus: valid.SuccessStatus}}},
		{name: "unsupported method", operations: []HTTPOperation{{Name: "edit", Method: http.MethodPut, Path: "/api/items", SuccessStatus: http.StatusOK}}},
		{name: "unsupported success", operations: []HTTPOperation{{Name: "created", Method: http.MethodPost, Path: "/api/items", SuccessStatus: http.StatusCreated}}},
		{name: "body on no content", operations: []HTTPOperation{{Name: "close", Method: http.MethodPost, Path: "/api/items", SuccessStatus: http.StatusNoContent, Output: reflect.TypeFor[httpProjectionSummary]()}}},
		{name: "dynamic path", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: "/api/items/{id}", SuccessStatus: http.StatusOK, Input: valid.Input}}},
		{name: "relative path", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: "api/items", SuccessStatus: http.StatusOK}}},
		{name: "query in path", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: "/api/items?id=one", SuccessStatus: http.StatusOK}}},
		{name: "network path", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: "//other.invalid/items", SuccessStatus: http.StatusOK}}},
		{name: "dot segment", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: "/api/../items", SuccessStatus: http.StatusOK}}},
		{name: "backslash path", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: "/api\\items", SuccessStatus: http.StatusOK}}},
		{name: "encoded dot segment", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: "/api/%2e%2e/items", SuccessStatus: http.StatusOK}}},
		{name: "optional query member", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: valid.Path, SuccessStatus: http.StatusOK, Input: reflect.TypeFor[httpOptionalQuery]()}}},
		{name: "numeric query member", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: valid.Path, SuccessStatus: http.StatusOK, Input: reflect.TypeFor[httpNumericQuery]()}}},
		{name: "unsupported output", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: valid.Path, SuccessStatus: http.StatusOK, Output: reflect.TypeFor[func()]()}}},
		{name: "unsupported response model", operations: []HTTPOperation{{Name: "lookup", Method: http.MethodGet, Path: valid.Path, SuccessStatus: http.StatusOK, Output: reflect.TypeFor[numericWireEnum]()}}},
		{name: "reserved type", operations: []HTTPOperation{valid}, roots: []reflect.Type{reflect.TypeFor[HTTPInputs]()}},
		{name: "enum and table binding", operations: []HTTPOperation{valid}, roots: []reflect.Type{reflect.TypeFor[wire]()}},
		{name: "enum and matcher binding", operations: []HTTPOperation{valid}, roots: []reflect.Type{reflect.TypeFor[matchesWire](), reflect.TypeFor[Values]()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const original = "previous artifact"
			var out strings.Builder
			out.WriteString(original)
			if err := WriteTypeScriptHTTP(&out, test.operations, test.roots...); err == nil {
				t.Fatal("invalid HTTP contract was accepted")
			}
			if out.String() != original {
				t.Fatalf("rejected configuration changed the published artifact: %q", out.String())
			}
		})
	}
}
