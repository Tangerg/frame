package browser

import (
	jsonv1 "encoding/json"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"slices"
	"strings"

	"github.com/Tangerg/frame/contractgen"
	framevalue "github.com/Tangerg/frame/value"
)

// Typed captures and response admission use the same Go declarations. A JSON
// round trip cannot establish a protocol type or settle an operation by itself.
func (e *typeScriptEmitter) writeProtocolDecoders() error {
	if err := e.reserveValues("control decoders", "protocolValues", "decodeProtocolValue", "controlOutputs", "decodeControlOutput", "isControlProblem"); err != nil {
		return err
	}
	if err := e.writeWireMatchers(); err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(e.types))
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

func (e *typeScriptEmitter) writeWireMatchers() error {
	var matchers strings.Builder
	for _, name := range slices.Sorted(maps.Keys(e.types)) {
		if err := e.reserveValues("wire matcher "+name, "matchesWire"+name); err != nil {
			return err
		}
		declaration := e.types[name]
		check, err := e.validateWireModel(declaration.source, declaration.model, "value")
		if err != nil {
			return err
		}
		fmt.Fprintf(&matchers, "function matchesWire%s(value: unknown): value is %s {\n  return %s;\n}\n\n", name, name, check)
	}
	if e.needsIntegerChecks {
		if err := e.reserveValues("integer validation", "isJSONNumber", "isJSONInteger", "compareJSONNumbers", "wireInteger"); err != nil {
			return err
		}
		e.WriteString(`import { isJSONNumber, isJSONInteger, compareJSONNumbers } from "@frame/sdk/json";

function wireInteger(value: unknown, minimum: bigint, maximum: bigint): boolean {
  return isJSONNumber(value) && isJSONInteger(value) &&
    compareJSONNumbers(value, minimum) >= 0 && compareJSONNumbers(value, maximum) <= 0;
}

`)
	} else if e.needsNumberChecks {
		if err := e.reserveValues("number validation", "isJSONNumber"); err != nil {
			return err
		}
		e.WriteString("import { isJSONNumber } from \"@frame/sdk/json\";\n\n")
	}
	if e.needsObjectChecks {
		if err := e.reserveValues("wire validation", "wireObject"); err != nil {
			return err
		}
		e.WriteString(`function wireObject(value: unknown, members: readonly string[]): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).every((name) => members.includes(name));
}

`)
	}
	if e.needsArrayChecks {
		if err := e.reserveValues("wire validation", "wireArray"); err != nil {
			return err
		}
		e.WriteString(`function wireArray(value: unknown, matches: (item: unknown) => boolean): value is readonly unknown[] {
  if (!Array.isArray(value)) return false;
  for (let index = 0; index < value.length; index++) {
    if (!Object.hasOwn(value, index) || !matches(value[index])) return false;
  }
  return true;
}

`)
	}
	if e.needsDigestCheck {
		if err := e.reserveValues("wire validation", "wireDigest"); err != nil {
			return err
		}
		fmt.Fprintf(e, "const wireDigest = new RegExp(%s);\n\n", contractgen.TypeScriptStringLiteral(framevalue.DigestPattern()))
	}
	e.WriteString(matchers.String())
	return nil
}

func (e *typeScriptEmitter) validateWireType(t reflect.Type, value string) (string, error) {
	if t.Kind() == reflect.Pointer {
		return e.validateWireType(t.Elem(), value)
	}
	if t == reflect.TypeFor[jsonv1.Number]() {
		e.needsNumberChecks = true
		return "isJSONNumber(" + value + ")", nil
	}
	if t.PkgPath() != "" && t.Name() != "" {
		return "matchesWire" + typeScriptTypeName(t) + "(" + value + ")", nil
	}
	return e.validateWireModel(t, t, value)
}

func (e *typeScriptEmitter) validateWireModel(source, model reflect.Type, value string) (string, error) {
	name := typeScriptTypeName(source)
	if declaration, exists := e.types[name]; exists && declaration.enumeration != nil {
		return fmt.Sprintf("typeof %s === \"string\" && %s%s.includes(%s as %s)", value, name, enumValuesSuffix, value, name), nil
	}
	if source == reflect.TypeFor[framevalue.Digest]() {
		e.needsDigestCheck = true
		return fmt.Sprintf("typeof %s === \"string\" && wireDigest.test(%s)", value, value), nil
	}
	if model == reflect.TypeFor[jsonv1.Number]() {
		e.needsNumberChecks = true
		return "isJSONNumber(" + value + ")", nil
	}
	switch model.Kind() {
	case reflect.Int8, reflect.Uint8, reflect.Int16, reflect.Uint16, reflect.Int32, reflect.Uint32,
		reflect.Int, reflect.Uint, reflect.Int64, reflect.Uint64:
		minimum, maximum := integerBounds(model)
		if model.Kind() != reflect.Int && model.Kind() != reflect.Uint && model.Bits() <= 32 {
			return fmt.Sprintf("typeof %s === \"number\" && Number.isInteger(%s) && %s >= %s && %s <= %s", value, value, value, minimum, value, maximum), nil
		}
		e.needsIntegerChecks = true
		return fmt.Sprintf("wireInteger(%s, %sn, %sn)", value, minimum, maximum), nil
	case reflect.String:
		return "typeof " + value + " === \"string\"", nil
	case reflect.Bool:
		return "typeof " + value + " === \"boolean\"", nil
	case reflect.Interface:
		return value + " !== undefined", nil
	case reflect.Slice, reflect.Array:
		e.needsArrayChecks = true
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
	e.needsObjectChecks = true
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

func integerBounds(model reflect.Type) (string, string) {
	limit := new(big.Int).Lsh(big.NewInt(1), uint(model.Bits()))
	minimum := new(big.Int)
	if model.Kind() >= reflect.Int && model.Kind() <= reflect.Int64 {
		limit.Rsh(limit, 1)
		minimum.Neg(limit)
	}
	maximum := new(big.Int).Sub(limit, big.NewInt(1))
	return minimum.String(), maximum.String()
}
