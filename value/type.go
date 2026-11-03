package value

import (
	"encoding"
	jsonv1 "encoding/json"
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
	if target.Kind() == reflect.Interface && target.NumMethod() != 0 {
		return fmt.Errorf("json: %s requires a concrete value type", target)
	}
	if opaqueType(target) {
		return nil
	}
	encoded, decoded := customEncoder(target), customDecoder(target)
	if encoded || decoded {
		if !encoded || !decoded || !target.Implements(reflect.TypeFor[interface{ JSONSchemaModel() any }]()) {
			return fmt.Errorf("json: %s requires an encoder, decoder and value-receiver JSONSchemaModel", target)
		}
		return nil
	}
	return validateShape(target, visited)
}

// validateShape admits a type whose JSON shape follows from its kind alone.
func validateShape(target reflect.Type, visited map[reflect.Type]bool) error {
	switch target.Kind() {
	case reflect.Slice, reflect.Array:
		if target.Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("json: %s requires an explicit string or array encoding", target)
		}
		return validateType(target.Elem(), visited)
	case reflect.Map:
		if target.Key().Kind() != reflect.String {
			return fmt.Errorf("json: %s requires string object keys", target)
		}
		if customEncoder(target.Key()) || customDecoder(target.Key()) {
			return fmt.Errorf("json: %s requires string keys without custom codecs", target)
		}
		return validateType(target.Elem(), visited)
	case reflect.Struct:
		fields, err := declaredFields(target, make(map[reflect.Type]bool))
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
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return nil
	case reflect.Float32, reflect.Float64:
		return fmt.Errorf("json: %s cannot represent exact JSON numbers; use value.Number", target)
	default:
		return fmt.Errorf("json: unsupported value type %s", target)
	}
}

func opaqueType(target reflect.Type) bool {
	return target == reflect.TypeFor[jsontext.Value]() || target == reflect.TypeFor[jsonv1.RawMessage]() || target == reflect.TypeFor[JSON]() || target.Kind() == reflect.Interface
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

func declaredFields(owner reflect.Type, visiting map[reflect.Type]bool) ([]Field, error) {
	if owner == nil || owner.Kind() != reflect.Struct {
		return nil, nil
	}
	if visiting[owner] {
		return nil, fmt.Errorf("json: recursive embedding of %s", owner)
	}
	visiting[owner] = true
	defer delete(visiting, owner)
	var fields []Field
	for field := range owner.Fields() {
		name, options, err := parseFieldTag(field)
		if err != nil {
			return nil, fmt.Errorf("json field %s.%s: %w", owner, field.Name, err)
		}
		if options.embedded {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				// Fresh decoding cannot allocate through an unexported pointer field.
				if !field.IsExported() {
					return nil, fmt.Errorf("json field %s.%s: embedded pointer must be exported", owner, field.Name)
				}
				embedded = embedded.Elem()
			}
			if embedded.Kind() != reflect.Struct || customEncoder(embedded) || customDecoder(embedded) {
				return nil, fmt.Errorf("json field %s.%s: embedding requires a plain struct", owner, field.Name)
			}
			children, err := declaredFields(embedded, visiting)
			if err != nil {
				return nil, err
			}
			fields = append(fields, children...)
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
			return nil, fmt.Errorf("json: ambiguous field %s.%s", owner, field.Name)
		}
		names[field.Name], selectors[field.GoName] = true, true
	}
	return fields, nil
}
