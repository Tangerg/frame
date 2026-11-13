package value

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// Decoding and generated projections must admit the same field ownership.
// Shadowed fields are refused instead of choosing a competing declaration.
type Field struct {
	Name string
	Type reflect.Type

	GoName   string
	Optional bool
}

func Fields(owner reflect.Type) []Field {
	if err := ValidateType(owner); err != nil {
		panic(err)
	}
	fields, err := declaredFields(owner, make(map[reflect.Type]bool))
	if err != nil {
		panic(err)
	}
	return fields
}

type wireTagOptions struct {
	embedded  bool
	omitEmpty bool
	omitZero  bool
}

const (
	omitEmptyOption = "omitempty"
	omitZeroOption  = "omitzero"
)

func parseFieldTag(field reflect.StructField) (string, wireTagOptions, error) {
	tag, tagged := field.Tag.Lookup("json")
	if tag == "-" {
		return "", wireTagOptions{}, nil
	}
	name, rest, hasOptions := strings.Cut(tag, ",")
	options := wireTagOptions{embedded: field.Anonymous && name == ""}
	if !field.IsExported() && !options.embedded {
		if tagged {
			return "", wireTagOptions{}, errors.New("unexported field must be ignored")
		}
		return "", wireTagOptions{}, nil
	}
	if !utf8.ValidString(name) || strings.ContainsAny(name, "\\'\"`") {
		return "", wireTagOptions{}, fmt.Errorf("invalid JSON field name %q", name)
	}
	if hasOptions {
		if err := options.parseOmission(rest); err != nil {
			return "", wireTagOptions{}, err
		}
		if options.embedded {
			return "", wireTagOptions{}, errors.New("embedded field cannot declare omission options")
		}
	}
	if err := checkSchemaFieldShape(field); err != nil {
		return "", wireTagOptions{}, err
	}
	if options.omitZero && !options.omitEmpty && (field.Type.Kind() == reflect.Pointer || field.Type.Kind() == reflect.Interface) {
		encoding, err := encodingOf(field.Type)
		if err != nil {
			return "", wireTagOptions{}, err
		}
		// omitempty intentionally omits JSON null. With omitzero alone, decoding
		// null to nil would erase presence before the value's codec can retain it.
		if encoding.model.Kind() == reflect.Interface {
			return "", wireTagOptions{}, errors.New("omitzero cannot preserve null in a pointer or interface field")
		}
	}
	if options.embedded {
		return "", options, nil
	}
	if name == "" {
		name = field.Name
	}
	return name, options, nil
}

// Schema tags may narrow a field, but cannot publish an encoding the field's
// codec cannot consume.
// Nullability belongs to the encoding model. A redundant nullable tag adds a
// competing oneOf branch that rejects null when the model already admits it.
const nullableSchemaOption = "nullable"

func checkSchemaFieldShape(field reflect.StructField) error {
	raw := field.Tag.Get("jsonschema")
	if first, _, _ := strings.Cut(raw, ","); first == "-" {
		return errors.New("schema cannot omit an encoded field")
	}
	continued := false
	for tag := range strings.SplitSeq(raw, ",") {
		if !continued && tag == nullableSchemaOption {
			return errors.New("schema nullability must come from the encoding model")
		}
		name, _, hasValue := strings.Cut(tag, "=")
		if hasValue && !continued {
			switch name {
			case "type", "oneof_ref", "anyof_ref", "oneof_type", "anyof_type":
				return fmt.Errorf("schema option %q cannot redefine a typed field", name)
			}
		}
		continued = strings.HasSuffix(tag, "\\")
	}
	return nil
}

func (options *wireTagOptions) parseOmission(raw string) error {
	for option := range strings.SplitSeq(raw, ",") {
		var duplicate bool
		switch option {
		case omitEmptyOption:
			duplicate, options.omitEmpty = options.omitEmpty, true
		case omitZeroOption:
			duplicate, options.omitZero = options.omitZero, true
		case "":
			return errors.New("empty field option")
		default:
			return fmt.Errorf("unsupported field option %q", option)
		}
		if duplicate {
			return fmt.Errorf("duplicate field option %q", option)
		}
	}
	return nil
}

// Shape is the member set of a JSON object type, the projection another
// runtime uses to admit exactly the members its Go owner declares.
type Shape struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

func ShapeOf(owner reflect.Type) Shape {
	shape := Shape{Required: []string{}, Optional: []string{}}
	for _, field := range Fields(owner) {
		if field.Optional {
			shape.Optional = append(shape.Optional, field.Name)
		} else {
			shape.Required = append(shape.Required, field.Name)
		}
	}
	return shape
}

func (s Shape) Admits(present []string) bool {
	required := 0
	for index, name := range present {
		if slices.Contains(present[:index], name) {
			return false
		}
		switch {
		case slices.Contains(s.Required, name):
			required++
		case !slices.Contains(s.Optional, name):
			return false
		}
	}
	return required == len(s.Required)
}
