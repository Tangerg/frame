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

// Typed captures and response admission use the same Go declarations. A JSON
// round trip cannot establish a protocol type or settle an operation by itself.
func (e *typeScriptEmitter) writeProtocolDecoders() error {
	e.WriteString(`function wireObject(value: unknown, members: readonly string[]): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).every((name) => members.includes(name));
}

function wireArray(value: unknown, matches: (item: unknown) => boolean): value is readonly unknown[] {
  if (!Array.isArray(value)) return false;
  for (let index = 0; index < value.length; index++) {
    if (!Object.hasOwn(value, index) || !matches(value[index])) return false;
  }
  return true;
}

const wireDigest = new RegExp(DIGEST_PATTERN);

`)
	names := slices.Sorted(maps.Keys(e.types))
	for _, name := range names {
		declaration := e.types[name]
		check, err := e.validateWireModel(declaration.source, declaration.model, "value")
		if err != nil {
			return err
		}
		fmt.Fprintf(e, "function matchesWire%s(value: unknown): value is %s {\n  return %s;\n}\n\n", name, name, check)
	}
	e.WriteString("export interface ProtocolValues {\n")
	for _, name := range names {
		fmt.Fprintf(e, "  readonly %s: %s;\n", contractgen.PropertyKey(name), name)
	}
	e.WriteString("}\n\nconst protocolValues = {\n")
	for _, name := range names {
		fmt.Fprintf(e, "  %s: matchesWire%s,\n", contractgen.RuntimePropertyKey(name), name)
	}
	e.WriteString(`} as const;

export function decodeProtocolValue<K extends keyof ProtocolValues>(name: K, value: unknown): ProtocolValues[K] {
  if (!Object.hasOwn(protocolValues, name) || !protocolValues[name](value))
    throw new TypeError("invalid protocol value");
  return value as ProtocolValues[K];
}

`)
	e.WriteString("const controlOutputs = {\n")
	for _, method := range controlMethods {
		check := "value === null"
		if method.output != nil {
			var err error
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
		check := "wireArray(" + value + ", (item: unknown) => " + item + ")"
		if model.Kind() == reflect.Array {
			check += fmt.Sprintf(" && %s.length === %d", value, model.Len())
		}
		return check, nil
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
