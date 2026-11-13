package browser

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/contractgen"
	framevalue "github.com/Tangerg/frame/value"
)

// The application binds its handlers from these same declarations. Projection
// must not introduce a separately maintained client schema or response type.
type HTTPOperation struct {
	Name          string
	Method        string
	Path          string
	SuccessStatus int
	Input         reflect.Type
	Output        reflect.Type
}

func WriteTypeScriptHTTP(out io.Writer, operations []HTTPOperation, roots ...reflect.Type) error {
	if len(operations) == 0 {
		return fmt.Errorf("http projection: operations are required")
	}
	roots = slices.Clone(roots)
	seen := make(map[string]bool, len(operations))
	patterns := make(map[string]bool, len(operations))
	for _, operation := range operations {
		if err := operation.validate(); err != nil {
			return err
		}
		pattern := operation.Method + " " + operation.Path
		if seen[operation.Name] || patterns[pattern] {
			return fmt.Errorf("http projection: duplicate operation %q", operation.Name)
		}
		seen[operation.Name], patterns[pattern] = true, true
		for _, model := range []reflect.Type{operation.Input, operation.Output} {
			if model != nil {
				roots = append(roots, model)
			}
		}
	}
	emitter, err := newTypeScriptEmitter(roots)
	if err != nil {
		return err
	}
	for _, reserved := range []string{"HTTPInputs", "HTTPOutputs", "WireValues"} {
		if _, exists := emitter.types[reserved]; exists {
			return fmt.Errorf("http projection: generated name %s is reserved", reserved)
		}
	}
	emitter.WriteString(generatedHeader)
	if err := emitter.writeHTTPOperations(operations); err != nil {
		return err
	}
	return emitter.finish(out)
}

func (o HTTPOperation) validate() error {
	if !frame.ValidName(o.Name) || !strings.HasPrefix(o.Path, "/") || strings.ContainsAny(o.Path, "{}?#\\ \t\r\n") {
		return fmt.Errorf("http projection: invalid operation %q", o.Name)
	}
	parsed, err := url.ParseRequestURI(o.Path)
	if err != nil || strings.HasPrefix(o.Path, "//") || parsed.Path != path.Clean(parsed.Path) || strings.ContainsAny(parsed.Path, "\\{}") {
		return fmt.Errorf("http projection: %s requires a canonical absolute path", o.Name)
	}
	if o.Method != http.MethodGet && o.Method != http.MethodPost {
		return fmt.Errorf("http projection: %s requires a GET or POST binding", o.Name)
	}
	if o.SuccessStatus != http.StatusOK && o.SuccessStatus != http.StatusNoContent {
		return fmt.Errorf("http projection: %s requires a JSON response or no-content acknowledgement", o.Name)
	}
	if o.SuccessStatus == http.StatusNoContent && o.Output != nil {
		return fmt.Errorf("http projection: %s has a body for a no-content acknowledgement", o.Name)
	}
	if o.Method == http.MethodGet {
		return o.validateQueryInputs()
	}
	return nil
}

func (o HTTPOperation) validateQueryInputs() error {
	if o.Input == nil {
		return nil
	}
	model, err := framevalue.EncodingModel(o.Input)
	if err != nil {
		return err
	}
	if model.Kind() != reflect.Struct {
		return fmt.Errorf("http projection: %s query input must be an object", o.Name)
	}
	for _, field := range framevalue.Fields(model) {
		if field.Optional || field.Type.Kind() != reflect.String {
			return fmt.Errorf("http projection: %s query input %s must be a required string", o.Name, field.Name)
		}
	}
	return nil
}

func (e *typeScriptEmitter) writeHTTPOperations(operations []HTTPOperation) error {
	if err := e.reserveValues("http projection", "httpOperations", "httpInputs", "httpOutputs", "httpRequestPath", "decodeHTTPOutput", "wireValues", "decodeWireValue"); err != nil {
		return err
	}
	var inputs, outputs strings.Builder
	for _, operation := range operations {
		inputCheck := "value === undefined"
		if operation.Input != nil {
			var err error
			inputCheck, err = e.validateWireType(operation.Input, "value")
			if err != nil {
				return err
			}
		}
		fmt.Fprintf(&inputs, "  %s: (value: unknown) => %s,\n", contractgen.RuntimePropertyKey(operation.Name), inputCheck)
		outputCheck, result := "value === null", "undefined"
		if operation.Output != nil {
			var err error
			outputCheck, err = e.validateWireType(operation.Output, "value")
			if err != nil {
				return err
			}
			result = "value"
		}
		fmt.Fprintf(&outputs, "  %s: (value: unknown) => {\n    if (!(%s)) throw new TypeError(\"invalid application response\");\n    return %s;\n  },\n", contractgen.RuntimePropertyKey(operation.Name), outputCheck, result)
	}
	if err := e.writeWireMatchers(); err != nil {
		return err
	}
	for _, projection := range []struct {
		name  string
		model func(HTTPOperation) reflect.Type
	}{{"HTTPInputs", func(o HTTPOperation) reflect.Type { return o.Input }}, {"HTTPOutputs", func(o HTTPOperation) reflect.Type { return o.Output }}} {
		fmt.Fprintf(e, "export interface %s {\n", projection.name)
		for _, operation := range operations {
			spelling, err := e.renderMessage(projection.model(operation))
			if err != nil {
				return err
			}
			fmt.Fprintf(e, "  readonly %s: %s;\n", contractgen.PropertyKey(operation.Name), spelling)
		}
		e.WriteString("}\n\n")
	}
	e.WriteString("export const httpOperations = {\n")
	for _, operation := range operations {
		fmt.Fprintf(e, "  %s: { method: %s, path: %s, status: %d },\n", contractgen.RuntimePropertyKey(operation.Name), contractgen.TypeScriptStringLiteral(operation.Method), contractgen.TypeScriptStringLiteral(operation.Path), operation.SuccessStatus)
	}
	fmt.Fprintf(e, "} as const;\n\nconst httpInputs = {\n%s} as const;\n\nconst httpOutputs = {\n%s} as const;\n\n", inputs.String(), outputs.String())
	e.WriteString(`export function httpRequestPath<K extends keyof HTTPInputs>(operation: K, input: NoInfer<HTTPInputs[K]>): string {
  if (!Object.hasOwn(httpInputs, operation) || !httpInputs[operation](input))
    throw new TypeError("invalid application request");
  const contract = httpOperations[operation];
  if (contract.method !== "GET" || input === undefined) return contract.path;
  const query = new URLSearchParams(Object.entries(input as object));
  return query.size === 0 ? contract.path : contract.path + "?" + query.toString();
}

export function decodeHTTPOutput<K extends keyof HTTPOutputs>(operation: K, value: unknown): HTTPOutputs[K] {
  if (!Object.hasOwn(httpOutputs, operation))
    throw new TypeError("unknown application operation");
  return httpOutputs[operation](value) as HTTPOutputs[K];
}

`)
	e.writeValueDecoders()
	return nil
}

func (e *typeScriptEmitter) writeValueDecoders() {
	names := slices.Sorted(maps.Keys(e.types))
	e.WriteString("export interface WireValues {\n")
	for _, name := range names {
		fmt.Fprintf(e, "  readonly %s: %s;\n", contractgen.PropertyKey(name), name)
	}
	e.WriteString("}\n\nconst wireValues = {\n")
	for _, name := range names {
		fmt.Fprintf(e, "  %s: matchesWire%s,\n", contractgen.RuntimePropertyKey(name), name)
	}
	e.WriteString(`} as const;

export function decodeWireValue<K extends keyof WireValues>(name: K, value: unknown): WireValues[K] {
  if (!Object.hasOwn(wireValues, name) || !wireValues[name](value))
    throw new TypeError("invalid application value");
  return value as WireValues[K];
}

`)
}
