package jsonvalue

import (
	"encoding"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
)

// Generated projections cannot reproduce shadowing or custom field encodings.
// Admit one explicit declaration before any consumer walks its field graph.
func ValidateType(target reflect.Type) error {
	return validateType(target, make(map[reflect.Type]bool))
}

func validateType(target reflect.Type, visited map[reflect.Type]bool) error {
	if target == nil {
		return errors.New("json: value type is required")
	}
	// Pointer cycles consume no JSON structure; field and element recursion does.
	pointers := make(map[reflect.Type]bool)
	for target.Kind() == reflect.Pointer {
		if customDecoder(target) {
			return nil
		}
		if pointers[target] {
			return fmt.Errorf("json: recursive pointer type %s has no value shape", target)
		}
		pointers[target] = true
		target = target.Elem()
	}
	if visited[target] {
		return nil
	}
	visited[target] = true
	if opaqueType(target) || customDecoder(target) {
		return nil
	}
	switch target.Kind() {
	case reflect.Slice, reflect.Array:
		return validateType(target.Elem(), visited)
	case reflect.Map:
		if target.Key().Kind() != reflect.String {
			return fmt.Errorf("json: %s requires string object keys", target)
		}
		return validateType(target.Elem(), visited)
	case reflect.Struct:
		fields, _, err := declaredFields(target, make(map[reflect.Type]bool))
		if err != nil {
			return err
		}
		for _, field := range fields {
			if err := validateType(field.Type, visited); err != nil {
				return fmt.Errorf("json field %s.%s: %w", target, field.Name, err)
			}
		}
		return nil
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return nil
	default:
		return fmt.Errorf("json: unsupported value type %s", target)
	}
}

func opaqueType(target reflect.Type) bool {
	return target == reflect.TypeFor[jsontext.Value]() || target == reflect.TypeFor[Value]() || target.Kind() == reflect.Interface
}

// TextEncoded reports a value that crosses JSON as its own text spelling, so a
// schema or type projection renders it as a string and never as its private
// representation.
func TextEncoded(target reflect.Type) bool {
	return target.Implements(reflect.TypeFor[encoding.TextMarshaler]()) &&
		reflect.PointerTo(target).Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
}

func customDecoder(target reflect.Type) bool {
	for _, candidate := range []reflect.Type{target, reflect.PointerTo(target)} {
		if candidate.Implements(reflect.TypeFor[json.Unmarshaler]()) ||
			candidate.Implements(reflect.TypeFor[json.UnmarshalerFrom]()) ||
			candidate.Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
			return true
		}
	}
	return false
}

func customEncoder(target reflect.Type) bool {
	for _, candidate := range []reflect.Type{target, reflect.PointerTo(target)} {
		if candidate.Implements(reflect.TypeFor[json.Marshaler]()) ||
			candidate.Implements(reflect.TypeFor[json.MarshalerTo]()) ||
			candidate.Implements(reflect.TypeFor[encoding.TextMarshaler]()) ||
			candidate.Implements(reflect.TypeFor[encoding.TextAppender]()) {
			return true
		}
	}
	return false
}

func declaredFields(owner reflect.Type, visiting map[reflect.Type]bool) ([]Field, []reflect.Type, error) {
	if owner == nil || owner.Kind() != reflect.Struct {
		return nil, nil, nil
	}
	if visiting[owner] {
		return nil, nil, fmt.Errorf("json: recursive embedding of %s", owner)
	}
	visiting[owner] = true
	defer delete(visiting, owner)
	var fields []Field
	var embeds []reflect.Type
	for field := range owner.Fields() {
		name, options, err := parseFieldTag(field)
		if err != nil {
			return nil, nil, fmt.Errorf("json field %s.%s: %w", owner, field.Name, err)
		}
		if options.embedded {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() != reflect.Struct || customEncoder(embedded) || customDecoder(embedded) {
				return nil, nil, fmt.Errorf("json field %s.%s: embedding requires a plain struct", owner, field.Name)
			}
			children, nested, err := declaredFields(embedded, visiting)
			if err != nil {
				return nil, nil, err
			}
			fields = append(fields, children...)
			embeds = append(embeds, embedded)
			embeds = append(embeds, nested...)
			continue
		}
		if name != "" {
			fields = append(fields, Field{Name: name, Type: field.Type, GoName: field.Name, Optional: options.optional})
		}
	}
	names := make(map[string]bool, len(fields))
	selectors := make(map[string]bool, len(fields))
	for _, field := range fields {
		if names[field.Name] || selectors[field.GoName] {
			return nil, nil, fmt.Errorf("json: ambiguous field %s.%s", owner, field.Name)
		}
		names[field.Name], selectors[field.GoName] = true, true
	}
	return fields, embeds, nil
}
