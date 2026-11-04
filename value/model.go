package value

import (
	jsonv1 "encoding/json"
	"fmt"
	"reflect"
)

type schemaModeler interface{ JSONSchemaModel() any }

// A codec's explicit model owns its wire shape. Its private Go representation
// must never become a competing declaration in generated consumers.
func EncodingModel(target reflect.Type) (reflect.Type, error) {
	if err := ValidateType(target); err != nil {
		return nil, err
	}
	seen := make(map[reflect.Type]bool)
	for {
		for target != nil && target.Kind() == reflect.Pointer {
			if seen[target] {
				return nil, fmt.Errorf("json: recursive encoding model for %s has no value shape", target)
			}
			seen[target] = true
			target = target.Elem()
		}
		if target == reflect.TypeFor[jsonv1.Number]() {
			return target, nil
		}
		if err := ValidateType(target); err != nil {
			return nil, err
		}
		if opaqueType(target) {
			return reflect.TypeFor[any](), nil
		}
		if !customEncoder(target) && !customDecoder(target) {
			return target, nil
		}
		if seen[target] {
			return nil, fmt.Errorf("json: recursive encoding model for %s has no value shape", target)
		}
		seen[target] = true
		model, err := encodingModel(target)
		if err != nil {
			return nil, err
		}
		target = model
	}
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
