package host

import (
	"fmt"
	"math"
	"reflect"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// PreparedMetadata keeps incoming wire metadata authoritative because a local
// Go decoder may project a different value.
type PreparedMetadata struct {
	contract       ContractRef
	projectionType reflect.Type
	value          framevalue.JSON
}

func (m PreparedMetadata) JSON() framevalue.JSON { return m.value }

func (m PreparedMetadata) forContract(definition *contractDefinition, metadataType reflect.Type) (framevalue.JSON, error) {
	if m.contract != definition.ref || m.projectionType != metadataType {
		return framevalue.JSON{}, frame.ErrContractMismatch
	}
	return m.value, nil
}

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

// publicationMetadata is the one admitted metadata of a publication. It is
// closed over wireMetadata, the validated wire document of a portable or JSON
// projection, and nativeMetadata, a detached Go snapshot. Readers always
// project a fresh value, so no reader can change what another observes.
type publicationMetadata interface {
	// readableAs reports whether a reader of type target can receive it; a
	// wire document is decoded by that reader's codec instead.
	readableAs(target reflect.Type) bool
	wireJSON() ([]byte, error)
}

type wireMetadata struct{ document framevalue.JSON }

// nativeMetadata may hold nil: an export without metadata.
type nativeMetadata struct{ snapshot any }

func (wireMetadata) readableAs(reflect.Type) bool { return true }

func (m wireMetadata) wireJSON() ([]byte, error) { return m.document.Bytes(), nil }

func (m nativeMetadata) readableAs(target reflect.Type) bool {
	return m.snapshot == nil || reflect.TypeOf(m.snapshot).AssignableTo(target)
}

func (m nativeMetadata) wireJSON() ([]byte, error) {
	detached, err := snapshotMetadata(m.snapshot)
	if err != nil {
		return nil, err
	}
	value, err := framevalue.EncodeJSON(detached)
	if err != nil {
		return nil, err
	}
	return value.Bytes(), nil
}

func readMetadata[M any](metadata publicationMetadata) (M, error) {
	var zero M
	switch metadata := metadata.(type) {
	case wireMetadata:
		if reflect.TypeFor[M]() == reflect.TypeFor[framevalue.JSON]() {
			return any(metadata.document).(M), nil
		}
		var decoded M
		if err := framevalue.DecodeValue(metadata.document.Bytes(), &decoded, "metadata"); err != nil {
			return zero, err
		}
		return decoded, nil
	case nativeMetadata:
		value, err := snapshotMetadata(metadata.snapshot)
		if err != nil || value == nil {
			return zero, err
		}
		typed, ok := value.(M)
		if !ok {
			return zero, frame.ErrContractMismatch
		}
		return typed, nil
	default:
		return zero, frame.ErrContractMismatch
	}
}

func snapshotMetadata(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	if admitted, ok := value.(framevalue.JSON); ok {
		return admitted, admitted.Validate()
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
			return reflect.Value{}, frame.ErrInvalid
		}
		return value, nil
	case reflect.Struct:
		return c.cloneStruct(value)
	case reflect.Array:
		return c.cloneElements(value, reflect.New(value.Type()).Elem())
	case reflect.Slice, reflect.Map:
		return c.cloneCollection(value)
	default:
		return reflect.Value{}, fmt.Errorf("%w: metadata contains a live or unsupported value", frame.ErrInvalid)
	}
}

func (c *metadataClone) cloneStruct(value reflect.Value) (reflect.Value, error) {
	result := reflect.New(value.Type()).Elem()
	for index := range value.NumField() {
		if !value.Type().Field(index).IsExported() {
			return reflect.Value{}, fmt.Errorf("%w: metadata contains inaccessible fields", frame.ErrInvalid)
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
		return reflect.Value{}, fmt.Errorf("%w: cyclic metadata", frame.ErrInvalid)
	}
	c.active[visit] = true
	defer delete(c.active, visit)
	if value.Kind() == reflect.Slice {
		return c.cloneElements(value, reflect.MakeSlice(value.Type(), value.Len(), value.Len()))
	}
	if value.Type().Key().Kind() != reflect.String {
		return reflect.Value{}, fmt.Errorf("%w: metadata map keys must be strings", frame.ErrInvalid)
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

func validateMetadata(definition *contractDefinition, metadataType reflect.Type, value framevalue.JSON) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if definition.metadataSchema != nil {
		if err := definition.metadataSchema.Validate(value.Bytes()); err != nil {
			return fmt.Errorf("point metadata: %w", err)
		}
	}
	if metadataType == nil || metadataType == reflect.TypeFor[framevalue.JSON]() {
		return nil
	}
	destination := reflect.New(metadataType)
	return framevalue.DecodeValue(value.Bytes(), destination.Interface(), "metadata")
}

func prepareMetadata(definition *contractDefinition, metadataType reflect.Type, metadata any) (publicationMetadata, error) {
	if prepared, ok := metadata.(PreparedMetadata); ok {
		document, err := prepared.forContract(definition, metadataType)
		return wireMetadata{document}, err
	}
	if definition.metadataSchema != nil {
		document, err := framevalue.EncodeJSON(metadata)
		if err != nil {
			return nil, err
		}
		return validatedWireMetadata(definition, metadataType, document)
	}
	if document, ok := metadata.(framevalue.JSON); ok {
		return validatedWireMetadata(definition, metadataType, document)
	}
	if metadataType != nil {
		if metadata == nil {
			if metadataType.Kind() != reflect.Interface {
				return nil, frame.ErrContractMismatch
			}
		} else if !reflect.TypeOf(metadata).AssignableTo(metadataType) {
			return nil, frame.ErrContractMismatch
		}
	}
	snapshot, err := snapshotMetadata(metadata)
	if err != nil {
		return nil, err
	}
	return nativeMetadata{snapshot}, nil
}

func validatedWireMetadata(definition *contractDefinition, metadataType reflect.Type, document framevalue.JSON) (publicationMetadata, error) {
	if err := validateMetadata(definition, metadataType, document); err != nil {
		return nil, err
	}
	return wireMetadata{document}, nil
}
