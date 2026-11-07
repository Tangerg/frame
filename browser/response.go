package browser

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/Tangerg/frame/portable/contractgen"
	framevalue "github.com/Tangerg/frame/value"
)

// Executable response checks come from the same Go declarations as their types.
// HTTP success alone cannot settle a mutation or authorize an object borrow.
func (e *typeScriptEmitter) writeResponseDecoders() error {
	roots := []reflect.Type{reflect.TypeFor[HTTPProblem]()}
	for _, method := range controlMethods {
		if method.output != nil {
			roots = append(roots, method.output)
		}
	}
	outputs, err := newTypeScriptEmitter(roots)
	if err != nil {
		return err
	}
	e.WriteString(`function wireObject(value: unknown, members: readonly string[]): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).every((name) => members.includes(name));
}

const wireDigest = new RegExp(DIGEST_PATTERN);

`)
	for _, name := range slices.Sorted(maps.Keys(outputs.types)) {
		declaration := outputs.types[name]
		check, err := e.validateWireModel(declaration.source, declaration.model, "value")
		if err != nil {
			return err
		}
		fmt.Fprintf(e, "function matchesWire%s(value: unknown): value is %s {\n  return %s;\n}\n\n", name, name, check)
	}
	e.WriteString("const controlOutputs = {\n")
	for _, method := range controlMethods {
		check := "value === null"
		if method.output != nil {
			check, err = e.validateWireType(method.output, "value")
			if err != nil {
				return err
			}
		}
		fmt.Fprintf(e, "  %s: (value: unknown) => %s,\n", contractgen.RuntimePropertyKey(string(method.name)), check)
	}
	e.WriteString(`} as const;

export function decodeControlOutput<K extends keyof ControlOutputs>(method: K, value: unknown): ControlOutputs[K] {
  if (!Object.hasOwn(controlOutputs, method) || !controlOutputs[method](value))
    throw new TypeError("invalid control response");
  return (value === null ? undefined : value) as ControlOutputs[K];
}

export function isControlProblem(value: unknown): value is HTTPProblem & { code: (typeof CONTROL_ERROR_CODES)[number] } {
  return matchesWireHTTPProblem(value) &&
    CONTROL_ERROR_CODES.includes(value.code as (typeof CONTROL_ERROR_CODES)[number]);
}

`)
	return nil
}

func (e *typeScriptEmitter) validateWireType(t reflect.Type, value string) (string, error) {
	if t.Kind() == reflect.Pointer {
		return e.validateWireType(t.Elem(), value)
	}
	if t.PkgPath() != "" && t.Name() != "" {
		return "matchesWire" + typeScriptTypeName(t) + "(" + value + ")", nil
	}
	return e.validateWireModel(t, t, value)
}

func (e *typeScriptEmitter) validateWireModel(source, model reflect.Type, value string) (string, error) {
	if _, enum := reflect.Zero(source).Interface().(interface{ Values() []string }); enum {
		if model.Kind() != reflect.String {
			return "", fmt.Errorf("control: string enum %s requires a string encoding model", source)
		}
		name := typeScriptTypeName(source)
		return fmt.Sprintf("typeof %s === \"string\" && %s%s.includes(%s as %s)", value, name, enumValuesSuffix, value, name), nil
	}
	if source == reflect.TypeFor[framevalue.Digest]() {
		return fmt.Sprintf("typeof %s === \"string\" && wireDigest.test(%s)", value, value), nil
	}
	switch model.Kind() {
	case reflect.String:
		return "typeof " + value + " === \"string\"", nil
	case reflect.Bool:
		return "typeof " + value + " === \"boolean\"", nil
	case reflect.Interface:
		return value + " !== undefined", nil
	case reflect.Slice, reflect.Array:
		item, err := e.validateWireType(model.Elem(), "item")
		if err != nil {
			return "", err
		}
		check := "Array.isArray(" + value + ")"
		if model.Kind() == reflect.Array {
			check += fmt.Sprintf(" && %s.length === %d", value, model.Len())
		}
		return check + " && " + value + ".every((item: unknown) => " + item + ")", nil
	case reflect.Struct:
		return e.validateWireFields(model, value)
	default:
		return "", fmt.Errorf("control: unsupported response model %s", model)
	}
}

func (e *typeScriptEmitter) validateWireFields(model reflect.Type, value string) (string, error) {
	fields := framevalue.Fields(model)
	names := make([]string, len(fields))
	checks := make([]string, 0, len(fields)+1)
	for i, field := range fields {
		names[i] = contractgen.TypeScriptStringLiteral(field.Name)
	}
	checks = append(checks, "wireObject("+value+", ["+strings.Join(names, ", ")+"])")
	for _, field := range fields {
		name := contractgen.TypeScriptStringLiteral(field.Name)
		check, err := e.validateWireType(field.Type, value+"["+name+"]")
		if err != nil {
			return "", err
		}
		presence := "Object.hasOwn(" + value + ", " + name + ")"
		if field.Optional {
			checks = append(checks, "(!"+presence+" || ("+check+"))")
		} else {
			checks = append(checks, presence, "("+check+")")
		}
	}
	return strings.Join(checks, " &&\n    "), nil
}
