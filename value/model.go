package value

import (
	jsonv1 "encoding/json"
	"errors"
	"fmt"
	"reflect"
)

type schemaModeler interface{ JSONSchemaModel() any }

// A codec's explicit model owns its wire shape. Its private Go representation
// must never become a competing declaration in generated consumers.
func EncodingModel(target reflect.Type) (reflect.Type, error) {
	return validateType(target, make(map[reflect.Type]bool))
}

func encodingType(target reflect.Type) (reflect.Type, error) {
	if target == nil {
		return nil, errors.New("json: value type is required")
	}
	seen := make(map[reflect.Type]bool)
	stringOnly := false
	for {
		if seen[target] {
			return nil, fmt.Errorf("json: recursive encoding model for %s has no value shape", target)
		}
		seen[target] = true
		if target.Kind() == reflect.Pointer {
			target = target.Elem()
			continue
		}
		if target.Kind() == reflect.Interface && target.NumMethod() != 0 {
			return nil, fmt.Errorf("json: %s requires a concrete value type", target)
		}
		if opaqueType(target) {
			target = reflect.TypeFor[any]()
			break
		}
		if target == reflect.TypeFor[jsonv1.Number]() {
			break
		}
		encoded, decoded := customEncoder(target), customDecoder(target)
		modeled := target.Implements(reflect.TypeFor[schemaModeler]())
		if !encoded && !decoded && !modeled && !reflect.PointerTo(target).Implements(reflect.TypeFor[schemaModeler]()) {
			break
		}
		if !encoded || !decoded || !modeled {
			return nil, fmt.Errorf("json: %s requires an encoder, decoder and value-receiver JSONSchemaModel", target)
		}
		stringOnly = stringOnly || !jsonEncoder(target) || !jsonDecoder(target)
		model, err := encodingModel(target)
		if err != nil {
			return nil, err
		}
		target = model
	}
	// Text codecs can only consume and produce JSON strings, even if their model
	// claims an opaque or numeric shape.
	if stringOnly && (target.Kind() != reflect.String || target == reflect.TypeFor[jsonv1.Number]()) {
		return nil, fmt.Errorf("json: text codec requires a string encoding model, got %s", target)
	}
	return target, nil
}

func encodingModel(target reflect.Type) (model reflect.Type, err error) {
	defer func() {
		if cause := recover(); cause != nil {
			if problem, ok := cause.(error); ok {
				err = fmt.Errorf("json: encoding model for %s panicked: %w", target, problem)
			} else {
				err = fmt.Errorf("json: encoding model for %s panicked: %v", target, cause)
			}
		}
	}()
	value := reflect.ValueOf(reflect.Zero(target).Interface().(schemaModeler).JSONSchemaModel())
	if !value.IsValid() {
		return nil, fmt.Errorf("json: encoding model for %s must be a non-nil value", target)
	}
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return nil, fmt.Errorf("json: encoding model for %s must be a non-nil value", target)
		}
	}
	return value.Type(), nil
}
