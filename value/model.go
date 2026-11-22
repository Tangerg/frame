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
		return wireEncoding{}, errors.New("value: value type is required")
	}
	chain := encodingChain{seen: make(map[reflect.Type]bool)}
	for more := true; more; {
		var err error
		if target, more, err = chain.step(target); err != nil {
			return wireEncoding{}, err
		}
	}
	// Text codecs can only consume and produce JSON strings, even if their model
	// claims an opaque or numeric shape.
	if chain.stringOnly && !stringModel(target) {
		return wireEncoding{}, fmt.Errorf("value: text codec requires a string encoding model, got %s", target)
	}
	if chain.enumerated != nil && !stringModel(target) {
		return wireEncoding{}, fmt.Errorf("value: string enum %s requires a string encoding model", chain.enumerated)
	}
	return wireEncoding{model: target, enumeration: chain.enumeration}, nil
}

// encodingChain accumulates the declarations a Go type passes through on its
// way to the model that owns its wire shape.
type encodingChain struct {
	seen        map[reflect.Type]bool
	stringOnly  bool
	enumerated  reflect.Type
	enumeration *StringEnum
}

// step advances one declaration and reports whether another model follows.
func (c *encodingChain) step(target reflect.Type) (reflect.Type, bool, error) {
	if c.seen[target] {
		return nil, false, fmt.Errorf("value: recursive encoding model for %s has no value shape", target)
	}
	c.seen[target] = true
	if target.Kind() == reflect.Pointer {
		return target.Elem(), true, nil
	}
	if err := c.admitEnum(target); err != nil {
		return nil, false, err
	}
	if target.Kind() == reflect.Interface && target.NumMethod() != 0 {
		return nil, false, fmt.Errorf("value: %s requires a concrete value type", target)
	}
	if opaqueType(target) {
		return reflect.TypeFor[any](), false, nil
	}
	if target == reflect.TypeFor[jsonv1.Number]() {
		return target, false, nil
	}
	return c.codecModel(target)
}

func (c *encodingChain) admitEnum(target reflect.Type) error {
	if !target.Implements(reflect.TypeFor[stringEnumerator]()) {
		if reflect.PointerTo(target).Implements(reflect.TypeFor[stringEnumerator]()) {
			return fmt.Errorf("value: %s requires a value-receiver JSONSchemaEnum", target)
		}
		return nil
	}
	members, err := enumMembers(target)
	if err != nil {
		return err
	}
	if c.enumeration == nil {
		c.enumeration = &StringEnum{members: slices.Clone(members)}
		c.enumerated = target
		return nil
	}
	c.enumeration.intersect(members)
	return nil
}

// codecModel follows an explicit codec to the model it declares. A type with
// none of encoder, decoder and model is its own wire shape.
func (c *encodingChain) codecModel(target reflect.Type) (reflect.Type, bool, error) {
	encoded, decoded := customEncoder(target), customDecoder(target)
	modeled := target.Implements(reflect.TypeFor[schemaModeler]())
	if !encoded && !decoded && !modeled && !reflect.PointerTo(target).Implements(reflect.TypeFor[schemaModeler]()) {
		return target, false, nil
	}
	if !encoded || !decoded || !modeled {
		return nil, false, fmt.Errorf("value: %s requires an encoder, decoder and value-receiver JSONSchemaModel", target)
	}
	c.stringOnly = c.stringOnly || !jsonEncoder(target) || !jsonDecoder(target)
	model, err := encodingModel(target)
	return model, true, err
}

func stringModel(target reflect.Type) bool {
	return target.Kind() == reflect.String && target != reflect.TypeFor[jsonv1.Number]()
}

func enumMembers(target reflect.Type) (members []string, err error) {
	defer func() {
		if cause := recover(); cause != nil {
			err = fmt.Errorf("value: enum vocabulary for %s panicked", target)
			if failure, ok := cause.(error); ok {
				err = errors.Join(err, failure)
			}
		}
	}()
	members = reflect.Zero(target).Interface().(stringEnumerator).JSONSchemaEnum()
	if len(members) == 0 {
		return nil, fmt.Errorf("value: enum vocabulary for %s must not be empty", target)
	}
	for index, member := range members {
		if !utf8.ValidString(member) || slices.Contains(members[:index], member) {
			return nil, fmt.Errorf("value: enum vocabulary for %s requires distinct UTF-8 strings", target)
		}
	}
	return members, nil
}

func encodingModel(target reflect.Type) (model reflect.Type, err error) {
	defer func() {
		if cause := recover(); cause != nil {
			err = fmt.Errorf("value: encoding model for %s panicked", target)
			if problem, ok := cause.(error); ok {
				err = errors.Join(err, problem)
			}
		}
	}()
	modelValue := reflect.Zero(target).Interface().(schemaModeler).JSONSchemaModel()
	if IsNil(modelValue) {
		return nil, fmt.Errorf("value: encoding model for %s must be a non-nil value", target)
	}
	return reflect.TypeOf(modelValue), nil
}
