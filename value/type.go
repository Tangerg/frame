package value

import (
	"encoding"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"reflect"
)

// Generated projections cannot reproduce shadowing or custom field encodings.
// Admit one explicit declaration before any consumer walks its field graph.
func ValidateType(target reflect.Type) error {
	_, err := validateType(target, make(map[reflect.Type]bool))
	return err
}

func validateType(target reflect.Type, visited map[reflect.Type]bool) (reflect.Type, error) {
	model, err := encodingType(target)
	if err != nil {
		return nil, err
	}
	if visited[model] {
		return model, nil
	}
	visited[model] = true
	if !opaqueType(model) {
		if err := validateShape(model, visited); err != nil {
			return nil, err
		}
	}
	return model, nil
}

func validateShape(target reflect.Type, visited map[reflect.Type]bool) error {
	switch target.Kind() {
	case reflect.Slice, reflect.Array:
		if target.Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("json: %s requires an explicit string or array encoding", target)
		}
		_, err := validateType(target.Elem(), visited)
		return err
	case reflect.Map:
		if target.Key().Kind() != reflect.String {
			return fmt.Errorf("json: %s requires string object keys", target)
		}
		if customEncoder(target.Key()) || customDecoder(target.Key()) {
			return fmt.Errorf("json: %s requires string keys without custom codecs", target)
		}
		_, err := validateType(target.Elem(), visited)
		return err
	case reflect.Struct:
		fields, err := declaredFields(target, make(map[reflect.Type]bool))
		if err != nil {
			return err
		}
		for _, field := range fields {
			if _, err := validateType(field.Type, visited); err != nil {
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
	return target == reflect.TypeFor[jsontext.Value]() || target == reflect.TypeFor[jsonv1.RawMessage]() || target.Kind() == reflect.Interface
}

// Nullability belongs to the encoding model, including a custom codec's model.
// Field omission is admitted separately because a nil value may lose presence.
func AllowsNull(target reflect.Type) bool {
	model, err := encodingType(target)
	return err == nil && model.Kind() == reflect.Interface
}

func customDecoder(target reflect.Type) bool {
	if jsonDecoder(target) {
		return true
	}
	return target.Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) || reflect.PointerTo(target).Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
}

func jsonDecoder(target reflect.Type) bool {
	for _, candidate := range []reflect.Type{target, reflect.PointerTo(target)} {
		if candidate.Implements(reflect.TypeFor[json.Unmarshaler]()) ||
			candidate.Implements(reflect.TypeFor[json.UnmarshalerFrom]()) {
			return true
		}
	}
	return false
}

func customEncoder(target reflect.Type) bool {
	if jsonEncoder(target) {
		return true
	}
	for _, candidate := range []reflect.Type{target, reflect.PointerTo(target)} {
		if candidate.Implements(reflect.TypeFor[encoding.TextMarshaler]()) ||
			candidate.Implements(reflect.TypeFor[encoding.TextAppender]()) {
			return true
		}
	}
	return false
}

func jsonEncoder(target reflect.Type) bool {
	for _, candidate := range []reflect.Type{target, reflect.PointerTo(target)} {
		if candidate.Implements(reflect.TypeFor[json.Marshaler]()) || candidate.Implements(reflect.TypeFor[json.MarshalerTo]()) {
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
	var hasJSONTag, hasJSONField bool
	for field := range owner.Fields() {
		_, tagged := field.Tag.Lookup("json")
		hasJSONTag = hasJSONTag || tagged
		name, options, err := parseFieldTag(field)
		if err != nil {
			return nil, fmt.Errorf("json field %s.%s: %w", owner, field.Name, err)
		}
		hasJSONField = hasJSONField || options.embedded || name != ""
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
			fields = append(fields, Field{Name: name, Type: field.Type, GoName: field.Name, Optional: options.omitEmpty || options.omitZero})
		}
	}
	if owner.NumField() != 0 && !hasJSONTag && !hasJSONField {
		return nil, fmt.Errorf("json: %s has no encoded fields; use an explicit codec or ignore its fields", owner)
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
