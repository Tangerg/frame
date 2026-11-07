package frame

import (
	"fmt"
	"math"
	"reflect"

	framevalue "github.com/Tangerg/frame/value"
)

// Metadata can share immutable subvalues, but an active traversal cycle cannot
// be detached from its live owner into a finite snapshot.
type metadataClone struct {
	active map[metadataVisit]bool
}

type metadataVisit struct {
	kind    reflect.Kind
	address uintptr
	length  int
}

// Portable metadata retains its admitted wire value. Typed views are decoded
// afresh; their pointers and codecs never become a second published value.
type portableMetadata struct {
	encoded framevalue.JSON
	target  reflect.Type
}

func readMetadata[M any](original any) (M, error) {
	var zero M
	value, err := snapshotMetadata(original)
	if err != nil || value == nil {
		return zero, err
	}
	metadata, ok := value.(M)
	if !ok {
		return zero, ErrContractMismatch
	}
	return metadata, nil
}

func (m portableMetadata) decoded() (any, error) {
	if m.target == nil || m.target == reflect.TypeFor[framevalue.JSON]() {
		return m.encoded, nil
	}
	destination := reflect.New(m.target)
	if err := framevalue.DecodeValue(m.encoded.Bytes(), destination.Interface(), "metadata"); err != nil {
		return nil, err
	}
	return destination.Elem().Interface(), nil
}

func snapshotMetadata(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	if admitted, ok := value.(framevalue.JSON); ok {
		return admitted, admitted.Validate()
	}
	if admitted, ok := value.(portableMetadata); ok {
		return admitted.decoded()
	}
	snapshot := metadataClone{active: make(map[metadataVisit]bool)}
	cloned, err := snapshot.clone(reflect.ValueOf(value))
	if err != nil {
		return nil, err
	}
	return cloned.Interface(), nil
}

func (c *metadataClone) clone(value reflect.Value) (reflect.Value, error) {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		inner, err := c.clone(value.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type()).Elem()
		result.Set(inner)
		return result, nil
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value, nil
	case reflect.Float32, reflect.Float64:
		if math.IsInf(value.Float(), 0) || math.IsNaN(value.Float()) {
			return reflect.Value{}, ErrInvalid
		}
		return value, nil
	case reflect.Struct:
		return c.cloneStruct(value)
	case reflect.Array:
		return c.cloneElements(value, reflect.New(value.Type()).Elem())
	case reflect.Slice, reflect.Map:
		return c.cloneCollection(value)
	default:
		return reflect.Value{}, fmt.Errorf("%w: metadata contains a live or unsupported value", ErrInvalid)
	}
}

func (c *metadataClone) cloneStruct(value reflect.Value) (reflect.Value, error) {
	result := reflect.New(value.Type()).Elem()
	for index := range value.NumField() {
		if !value.Type().Field(index).IsExported() {
			return reflect.Value{}, fmt.Errorf("%w: metadata contains inaccessible fields", ErrInvalid)
		}
		field, err := c.clone(value.Field(index))
		if err != nil {
			return reflect.Value{}, err
		}
		result.Field(index).Set(field)
	}
	return result, nil
}

func (c *metadataClone) cloneElements(value, result reflect.Value) (reflect.Value, error) {
	for index := range value.Len() {
		item, err := c.clone(value.Index(index))
		if err != nil {
			return reflect.Value{}, err
		}
		result.Index(index).Set(item)
	}
	return result, nil
}

func (c *metadataClone) cloneCollection(value reflect.Value) (reflect.Value, error) {
	if value.IsNil() {
		return reflect.Zero(value.Type()), nil
	}
	// A shared backing address does not make two different slice views cyclic.
	visit := metadataVisit{kind: value.Kind(), address: value.Pointer(), length: value.Len()}
	if c.active[visit] {
		return reflect.Value{}, fmt.Errorf("%w: cyclic metadata", ErrInvalid)
	}
	c.active[visit] = true
	defer delete(c.active, visit)
	if value.Kind() == reflect.Slice {
		return c.cloneElements(value, reflect.MakeSlice(value.Type(), value.Len(), value.Len()))
	}
	if value.Type().Key().Kind() != reflect.String {
		return reflect.Value{}, fmt.Errorf("%w: metadata map keys must be strings", ErrInvalid)
	}
	result := reflect.MakeMapWithSize(value.Type(), value.Len())
	iterator := value.MapRange()
	for iterator.Next() {
		item, err := c.clone(iterator.Value())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetMapIndex(iterator.Key(), item)
	}
	return result, nil
}

func decodeMetadata(definition *declaration, value framevalue.JSON) (any, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	if definition.metadataSchema != nil {
		if err := definition.metadataSchema.Validate(value.Bytes()); err != nil {
			return nil, fmt.Errorf("point metadata: %w", err)
		}
	}
	if definition.metadataType == nil || definition.metadataType == reflect.TypeFor[framevalue.JSON]() {
		return value, nil
	}
	destination := reflect.New(definition.metadataType)
	if err := framevalue.DecodeValue(value.Bytes(), destination.Interface(), "metadata"); err != nil {
		return nil, err
	}
	if definition.metadataSchema != nil {
		return destination.Elem().Interface(), nil
	}
	return snapshotMetadata(destination.Elem().Interface())
}

func prepareMetadata(definition *declaration, metadata any) (any, error) {
	if definition.metadataSchema != nil {
		value, err := framevalue.EncodeJSON(metadata)
		if err != nil {
			return nil, err
		}
		if _, err := decodeMetadata(definition, value); err != nil {
			return nil, err
		}
		return portableMetadata{encoded: value, target: definition.metadataType}, nil
	}
	if value, ok := metadata.(framevalue.JSON); ok {
		return decodeMetadata(definition, value)
	}
	if definition.metadataType != nil {
		if metadata == nil {
			if definition.metadataType.Kind() != reflect.Interface {
				return nil, ErrContractMismatch
			}
		} else if !reflect.TypeOf(metadata).AssignableTo(definition.metadataType) {
			return nil, ErrContractMismatch
		}
	}
	return snapshotMetadata(metadata)
}
