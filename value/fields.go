package value

import (
	"errors"
	"fmt"
	"reflect"
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
	embedded bool
	optional bool
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
	if err := checkSchemaFieldShape(field); err != nil {
		return "", wireTagOptions{}, err
	}
	if !utf8.ValidString(name) || strings.ContainsAny(name, "\\'\"`") {
		return "", wireTagOptions{}, fmt.Errorf("invalid JSON field name %q", name)
	}
	if hasOptions {
		optional, err := parseOmissionOptions(rest)
		if err != nil {
			return "", wireTagOptions{}, err
		}
		if options.embedded {
			return "", wireTagOptions{}, errors.New("embedded field cannot declare omission options")
		}
		options.optional = optional
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
func checkSchemaFieldShape(field reflect.StructField) error {
	raw := field.Tag.Get("jsonschema")
	if first, _, _ := strings.Cut(raw, ","); first == "-" {
		return errors.New("schema cannot omit an encoded field")
	}
	continued := false
	for tag := range strings.SplitSeq(raw, ",") {
		if tag == "nullable" {
			target := field.Type
			pointers := make(map[reflect.Type]bool)
			for target.Kind() == reflect.Pointer && !pointers[target] {
				pointers[target] = true
				target = target.Elem()
			}
			if !opaqueType(target) {
				return errors.New("nullable schema requires an opaque JSON field")
			}
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

func parseOmissionOptions(raw string) (bool, error) {
	var seenOmitEmpty, seenOmitZero bool
	for option := range strings.SplitSeq(raw, ",") {
		var duplicate bool
		switch option {
		case omitEmptyOption:
			duplicate, seenOmitEmpty = seenOmitEmpty, true
		case omitZeroOption:
			duplicate, seenOmitZero = seenOmitZero, true
		case "":
			return false, errors.New("empty field option")
		default:
			return false, fmt.Errorf("unsupported field option %q", option)
		}
		if duplicate {
			return false, fmt.Errorf("duplicate field option %q", option)
		}
	}
	return seenOmitEmpty || seenOmitZero, nil
}
