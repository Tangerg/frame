package value

import (
	jsonv1 "encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"unicode/utf8"
)

type schemaModeler interface{ JSONSchemaModel() any }
type stringEnumerator interface{ JSONSchemaEnum() []string }

// A codec may narrow its model's vocabulary; composition retains every
// declared constraint without giving either declaration override authority.
type StringEnum struct{ members []string }

func (e *StringEnum) Members() []string { return slices.Clone(e.members) }

func (e *StringEnum) contains(member string) bool { return slices.Contains(e.members, member) }

func (e *StringEnum) intersect(members []string) {
	e.members = slices.DeleteFunc(e.members, func(member string) bool { return !slices.Contains(members, member) })
}

type wireEncoding struct {
	model       reflect.Type
	enumeration *StringEnum
}

// EncodingEnum is absent only when no declaration in the encoding chain owns
// a vocabulary. An empty intersection remains a closed empty set.
func EncodingEnum(target reflect.Type) (*StringEnum, error) {
	encoding, err := encodingOf(target)
	return encoding.enumeration, err
}

// A codec's explicit model owns its wire shape. Its private Go representation
// must never become a competing declaration in generated consumers.
func EncodingModel(target reflect.Type) (reflect.Type, error) {
	return validateType(target, make(map[reflect.Type]bool))
}

func encodingOf(target reflect.Type) (wireEncoding, error) {
	if target == nil {
		return wireEncoding{}, errors.New("json: value type is required")
	}
	seen := make(map[reflect.Type]bool)
	stringOnly := false
	var enumerated reflect.Type
	var enumeration *StringEnum
	for {
		if seen[target] {
			return wireEncoding{}, fmt.Errorf("json: recursive encoding model for %s has no value shape", target)
		}
		seen[target] = true
		if target.Kind() == reflect.Pointer {
			target = target.Elem()
			continue
		}
		if target.Implements(reflect.TypeFor[stringEnumerator]()) {
			members, err := enumMembers(target)
			if err != nil {
				return wireEncoding{}, err
			}
			if enumeration == nil {
				enumeration = &StringEnum{members: slices.Clone(members)}
			} else {
				enumeration.intersect(members)
			}
			if enumerated == nil {
				enumerated = target
			}
		} else if reflect.PointerTo(target).Implements(reflect.TypeFor[stringEnumerator]()) {
			return wireEncoding{}, fmt.Errorf("json: %s requires a value-receiver JSONSchemaEnum", target)
		}
		if target.Kind() == reflect.Interface && target.NumMethod() != 0 {
			return wireEncoding{}, fmt.Errorf("json: %s requires a concrete value type", target)
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
			return wireEncoding{}, fmt.Errorf("json: %s requires an encoder, decoder and value-receiver JSONSchemaModel", target)
		}
		stringOnly = stringOnly || !jsonEncoder(target) || !jsonDecoder(target)
		model, err := encodingModel(target)
		if err != nil {
			return wireEncoding{}, err
		}
		target = model
	}
	// Text codecs can only consume and produce JSON strings, even if their model
	// claims an opaque or numeric shape.
	if stringOnly && (target.Kind() != reflect.String || target == reflect.TypeFor[jsonv1.Number]()) {
		return wireEncoding{}, fmt.Errorf("json: text codec requires a string encoding model, got %s", target)
	}
	if enumerated != nil && (target.Kind() != reflect.String || target == reflect.TypeFor[jsonv1.Number]()) {
		return wireEncoding{}, fmt.Errorf("json: string enum %s requires a string encoding model", enumerated)
	}
	return wireEncoding{model: target, enumeration: enumeration}, nil
}

func enumMembers(target reflect.Type) (members []string, err error) {
	defer func() {
		if cause := recover(); cause != nil {
			err = fmt.Errorf("json: enum vocabulary for %s panicked", target)
			if failure, ok := cause.(error); ok {
				err = errors.Join(err, failure)
			}
		}
	}()
	members = reflect.Zero(target).Interface().(stringEnumerator).JSONSchemaEnum()
	if len(members) == 0 {
		return nil, fmt.Errorf("json: enum vocabulary for %s must not be empty", target)
	}
	for index, member := range members {
		if !utf8.ValidString(member) || slices.Contains(members[:index], member) {
			return nil, fmt.Errorf("json: enum vocabulary for %s requires distinct UTF-8 strings", target)
		}
	}
	return members, nil
}

func encodingModel(target reflect.Type) (model reflect.Type, err error) {
	defer func() {
		if cause := recover(); cause != nil {
			err = fmt.Errorf("json: encoding model for %s panicked", target)
			if problem, ok := cause.(error); ok {
				err = errors.Join(err, problem)
			}
		}
	}()
	modelValue := reflect.Zero(target).Interface().(schemaModeler).JSONSchemaModel()
	if IsNil(modelValue) {
		return nil, fmt.Errorf("json: encoding model for %s must be a non-nil value", target)
	}
	return reflect.TypeOf(modelValue), nil
}
