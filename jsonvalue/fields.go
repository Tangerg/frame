package jsonvalue

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
	fields, _, err := declaredFields(owner, make(map[reflect.Type]bool))
	if err != nil {
		panic(err)
	}
	return fields
}

func Embeds(owner reflect.Type) []reflect.Type {
	if err := ValidateType(owner); err != nil {
		panic(err)
	}
	_, embedded, err := declaredFields(Deref(owner), make(map[reflect.Type]bool))
	if err != nil {
		panic(err)
	}
	return embedded
}

func FieldNames(owner reflect.Type) []string {
	fields := Fields(owner)
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		out = append(out, field.Name)
	}
	return out
}

func LookupField(owner reflect.Type, name string) (Field, bool) {
	for _, field := range Fields(owner) {
		if field.Name == name {
			return field, true
		}
	}
	return Field{}, false
}

func HasPath(root reflect.Type, path string) error {
	current := root
	for segment := range strings.SplitSeq(path, ".") {
		field, ok := LookupField(current, segment)
		if !ok {
			return fmt.Errorf("no JSON field %q on %s", segment, current.Name())
		}
		current = Deref(field.Type)
	}
	return nil
}

// Callers that project Go expressions need the full path because a pointer or
// collection parent cannot be read by the same direct selector as a value struct.
func PathFields(root reflect.Type, path string) ([]Field, bool) {
	current := root
	var fields []Field
	for segment := range strings.SplitSeq(path, ".") {
		field, found := LookupField(current, segment)
		if !found {
			return nil, false
		}
		fields = append(fields, field)
		current = Deref(field.Type)
	}
	return fields, len(fields) > 0
}

func GoPath(root reflect.Type, path string) (selector string, leaf Field, ok bool) {
	fields, ok := PathFields(root, path)
	if !ok {
		return "", Field{}, false
	}
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, field.GoName)
	}
	return strings.Join(parts, "."), fields[len(fields)-1], true
}

func Deref(t reflect.Type) reflect.Type {
	seen := make(map[reflect.Type]bool)
	for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice) && !seen[t] {
		seen[t] = true
		t = t.Elem()
	}
	return t
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
