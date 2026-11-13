package value

import (
	"bytes"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

// Decode into a fresh value so a refused shape cannot partially change its owner.
func DecodeValue(raw jsontext.Value, dst any, path string) error {
	destination := reflect.ValueOf(dst)
	if !destination.IsValid() || destination.Kind() != reflect.Pointer || destination.IsNil() {
		return fmt.Errorf("%s: destination must be a non-nil pointer", path)
	}
	target := destination.Type().Elem()
	if err := ValidateType(target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := validateValueShape(raw, target, ""); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	value := reflect.New(target)
	if err := json.Unmarshal(raw, value.Interface(), json.RejectUnknownMembers(true), Numbers()); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	destination.Elem().Set(value.Elem())
	return nil
}

// ShapeError keeps an authored field address available to protocol error projection.
type ShapeError struct {
	Field  string
	Detail string
}

func (e *ShapeError) Error() string {
	return e.Field + " " + e.Detail
}

// Admit the complete declared wire model before any codec can run. A codec's
// private representation cannot waive a sibling's presence or its own model.
func validateValueShape(raw jsontext.Value, target reflect.Type, path string) error {
	encoding, err := encodingOf(target)
	if err != nil {
		return err
	}
	model := encoding.model
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if encoding.enumeration != nil {
		var member string
		if err := json.Unmarshal(raw, &member); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		if !encoding.enumeration.contains(member) {
			return &ShapeError{Field: path, Detail: "is not a declared enum value"}
		}
	}
	if model.Kind() == reflect.Interface {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &ShapeError{Field: path, Detail: "must not be null"}
	}
	if model == reflect.TypeFor[jsonv1.Number]() {
		if raw.Kind() != '0' {
			return &ShapeError{Field: path, Detail: "must be a number"}
		}
		if err := json.Unmarshal(raw, new(jsonv1.Number), Numbers()); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		return nil
	}
	if customDecoder(target) {
		declared, err := encodingModel(target)
		if err != nil {
			return err
		}
		return validateValueShape(raw, declared, path)
	}

	switch target.Kind() {
	case reflect.Struct:
		return validateStructShape(raw, target, path)
	case reflect.Slice, reflect.Array:
		return validateSequenceShape(raw, target, path)
	case reflect.Map:
		return validateMapShape(raw, target.Elem(), path)
	}
	if err := json.Unmarshal(raw, reflect.New(target).Interface(), Numbers()); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func validateStructShape(raw jsontext.Value, target reflect.Type, path string) error {
	var object map[string]jsontext.Value
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	fields, err := declaredFields(target, make(map[reflect.Type]bool))
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(object)) {
		if !slices.ContainsFunc(fields, func(field Field) bool { return field.Name == name }) {
			return &ShapeError{Field: fieldPath(path, name), Detail: "is not a declared field"}
		}
	}
	for _, field := range fields {
		value, present := object[field.Name]
		if !present {
			if !field.Optional {
				return &ShapeError{Field: fieldPath(path, field.Name), Detail: "is required"}
			}
			continue
		}
		if err := validateValueShape(value, field.Type, fieldPath(path, field.Name)); err != nil {
			return err
		}
	}
	return nil
}

func validateSequenceShape(raw jsontext.Value, target reflect.Type, path string) error {
	var values []jsontext.Value
	if err := json.Unmarshal(raw, &values); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if target.Kind() == reflect.Array && len(values) != target.Len() {
		return &ShapeError{Field: path, Detail: fmt.Sprintf("must contain exactly %d values", target.Len())}
	}
	for index, value := range values {
		if err := validateValueShape(value, target.Elem(), fmt.Sprintf("%s[%d]", path, index)); err != nil {
			return err
		}
	}
	return nil
}

func validateMapShape(raw jsontext.Value, element reflect.Type, path string) error {
	var values map[string]jsontext.Value
	if err := json.Unmarshal(raw, &values); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if err := validateValueShape(values[key], element, MapPath(path, key)); err != nil {
			return err
		}
	}
	return nil
}

func fieldPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}

// MapPath quotes property names so a map key cannot alias a nested field path.
func MapPath(parent, key string) string {
	return fmt.Sprintf("%s[%q]", parent, key)
}
