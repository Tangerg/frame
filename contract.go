package frame

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"reflect"

	"github.com/Tangerg/frame/integrity"
	"github.com/Tangerg/frame/jsonvalue"
	"github.com/Tangerg/frame/portable"
)

type ContractKind string

const (
	ServiceKind ContractKind = "service"
	PointKind   ContractKind = "point"
	EventKind   ContractKind = "event"
)

type ContractMode string

const (
	NativeMode   ContractMode = "native"
	PortableMode ContractMode = "portable"
)

func (ContractKind) Values() []string {
	return []string{string(ServiceKind), string(PointKind), string(EventKind)}
}
func (ContractMode) Values() []string { return []string{string(NativeMode), string(PortableMode)} }

// ContractRef is a transport and persistence projection of a declaration. Its
// digest is derived from the original descriptor, not from implementation types.
type ContractRef struct {
	Name    string           `json:"name"`
	Version string           `json:"version"`
	Digest  integrity.Digest `json:"digest"`
	Kind    ContractKind     `json:"kind"`
	Mode    ContractMode     `json:"mode"`
}

func (r ContractRef) Validate() error {
	if err := validateName("contract name", r.Name); err != nil {
		return err
	}
	if err := validateName("contract version", r.Version); err != nil {
		return err
	}
	if err := r.Digest.Validate(); err != nil {
		return fmt.Errorf("%w: contract digest: %w", ErrInvalid, err)
	}
	if r.Kind != ServiceKind && r.Kind != PointKind && r.Kind != EventKind {
		return fmt.Errorf("%w: contract kind", ErrInvalid)
	}
	if r.Mode != NativeMode && r.Mode != PortableMode {
		return fmt.Errorf("%w: contract mode", ErrInvalid)
	}
	return nil
}

func (r ContractRef) String() string { return r.Name + "@" + r.Version }

// nominalContract is a contract's identity without its digest: one kind, name
// and version may bind only one digest within a Host.
type nominalContract struct {
	kind    ContractKind
	name    string
	version string
}

func contractName(ref ContractRef) nominalContract {
	return nominalContract{kind: ref.Kind, name: ref.Name, version: ref.Version}
}

type declaration struct {
	ref            ContractRef
	descriptor     []byte
	valueType      reflect.Type
	metadataType   reflect.Type
	valueContract  ContractRef
	metadataSchema *portable.Schema
}

// Declaration can only be produced by the typed constructors. Ref values
// received over a wire still require their original admitted declaration.
type Declaration interface {
	Ref() ContractRef
	definition() *declaration
}

type Contract[T any] struct{ value *declaration }
type Point[M, V any] struct{ value *declaration }
type Event[T any] struct{ value *declaration }

func (c Contract[T]) Ref() ContractRef {
	if c.value == nil {
		return ContractRef{}
	}
	return c.value.ref
}
func (p Point[M, V]) Ref() ContractRef {
	if p.value == nil {
		return ContractRef{}
	}
	return p.value.ref
}
func (e Event[T]) Ref() ContractRef {
	if e.value == nil {
		return ContractRef{}
	}
	return e.value.ref
}
func (c Contract[T]) definition() *declaration { return c.value }
func (p Point[M, V]) definition() *declaration { return p.value }
func (e Event[T]) definition() *declaration    { return e.value }

// Descriptor returns the original immutable identity document. Local Go types
// and driver proxy types are never included in its wire identity.
func (c Contract[T]) Descriptor() []byte { return bytes.Clone(c.value.descriptor) }
func (p Point[M, V]) Descriptor() []byte { return bytes.Clone(p.value.descriptor) }
func (e Event[T]) Descriptor() []byte    { return bytes.Clone(e.value.descriptor) }

// ValueContract identifies a portable Point's callable or data-value contract.
// A native Point binds V directly in its owning language and returns a zero ref.
func (p Point[M, V]) ValueContract() ContractRef {
	if p.value == nil {
		return ContractRef{}
	}
	return p.value.valueContract
}

// PointDescriptor keeps membership metadata separate from the exact value
// contract. A callable service descriptor is never relabelled as a Point.
type PointDescriptor struct {
	Protocol      string         `json:"protocol"`
	Name          string         `json:"name"`
	Version       string         `json:"version"`
	Metadata      jsontext.Value `json:"metadata"`
	ValueContract ContractRef    `json:"valueContract"`
}

const PointProtocol = "io.github.tangerg.frame/point/1"

func pointDeclaration(ref ContractRef, descriptor []byte) (*declaration, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if err := portable.CheckDescriptor(descriptor); err != nil {
		return nil, err
	}
	if ref.Kind != PointKind || ref.Mode != PortableMode || integrity.Hash(descriptor) != ref.Digest {
		return nil, ErrContractMismatch
	}
	var shape PointDescriptor
	if err := jsonvalue.DecodeValue(descriptor, &shape, "point"); err != nil {
		return nil, err
	}
	if shape.Protocol != PointProtocol || shape.Name != ref.Name || shape.Version != ref.Version {
		return nil, ErrContractMismatch
	}
	if err := shape.ValueContract.Validate(); err != nil {
		return nil, err
	}
	if shape.ValueContract.Kind != ServiceKind || shape.ValueContract.Mode != PortableMode {
		return nil, ErrContractMismatch
	}
	schema, err := portable.AdmitSchema(shape.Metadata)
	if err != nil {
		return nil, fmt.Errorf("point metadata: %w", err)
	}
	return &declaration{ref: ref, descriptor: bytes.Clone(descriptor), valueContract: shape.ValueContract, metadataSchema: &schema}, nil
}

func NewPortablePoint[M, V any](name, version string, metadataSchema []byte, value Contract[V]) (Point[M, V], error) {
	if value.value == nil || value.Ref().Mode != PortableMode {
		return Point[M, V]{}, ErrContractMismatch
	}
	if err := validateName("contract name", name); err != nil {
		return Point[M, V]{}, err
	}
	if err := validateName("contract version", version); err != nil {
		return Point[M, V]{}, err
	}
	if err := portable.CheckDescriptor(metadataSchema); err != nil {
		return Point[M, V]{}, err
	}
	shape := PointDescriptor{Protocol: PointProtocol, Name: name, Version: version, Metadata: bytes.Clone(metadataSchema), ValueContract: value.Ref()}
	identity, err := jsonvalue.Encode(shape)
	if err != nil {
		return Point[M, V]{}, err
	}
	ref := ContractRef{Name: name, Version: version, Digest: integrity.Hash(identity.JSON()), Kind: PointKind, Mode: PortableMode}
	return BindPoint[M, V](ref, identity.JSON())
}

func nativeDeclaration(name, version string, kind ContractKind, valueType, metadataType reflect.Type) (*declaration, error) {
	descriptor, err := json.Marshal(struct {
		Name    string       `json:"name"`
		Version string       `json:"version"`
		Kind    ContractKind `json:"kind"`
		Mode    ContractMode `json:"mode"`
	}{name, version, kind, NativeMode})
	if err != nil {
		return nil, err
	}
	ref := ContractRef{Name: name, Version: version, Digest: integrity.Hash(descriptor), Kind: kind, Mode: NativeMode}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	return &declaration{ref: ref, descriptor: descriptor, valueType: valueType, metadataType: metadataType}, nil
}

func NewContract[T any](name, version string) (Contract[T], error) {
	value, err := nativeDeclaration(name, version, ServiceKind, reflect.TypeFor[T](), nil)
	return Contract[T]{value: value}, err
}

func NewPoint[M, V any](name, version string) (Point[M, V], error) {
	value, err := nativeDeclaration(name, version, PointKind, reflect.TypeFor[V](), reflect.TypeFor[M]())
	return Point[M, V]{value: value}, err
}

func NewEvent[T any](name, version string) (Event[T], error) {
	value, err := nativeDeclaration(name, version, EventKind, reflect.TypeFor[T](), nil)
	return Event[T]{value: value}, err
}

// BindContract uses an already published portable descriptor. Local types do
// not derive another identity or alter the original contract's validation.
func BindContract[T any](ref ContractRef, descriptor []byte) (Contract[T], error) {
	if err := ref.Validate(); err != nil {
		return Contract[T]{}, err
	}
	if ref.Kind != ServiceKind || ref.Mode != PortableMode || integrity.Hash(descriptor) != ref.Digest {
		return Contract[T]{}, ErrContractMismatch
	}
	return Contract[T]{value: &declaration{ref: ref, descriptor: bytes.Clone(descriptor), valueType: reflect.TypeFor[T]()}}, nil
}

func BindPoint[M, V any](ref ContractRef, descriptor []byte) (Point[M, V], error) {
	value, err := pointDeclaration(ref, descriptor)
	if err != nil {
		return Point[M, V]{}, err
	}
	value.valueType = reflect.TypeFor[V]()
	value.metadataType = reflect.TypeFor[M]()
	return Point[M, V]{value: value}, nil
}

func BindEvent[T any](ref ContractRef, descriptor []byte) (Event[T], error) {
	if err := ref.Validate(); err != nil {
		return Event[T]{}, err
	}
	if ref.Kind != EventKind || ref.Mode != PortableMode || integrity.Hash(descriptor) != ref.Digest {
		return Event[T]{}, ErrContractMismatch
	}
	return Event[T]{value: &declaration{ref: ref, descriptor: bytes.Clone(descriptor), valueType: reflect.TypeFor[T]()}}, nil
}

func sameDeclaration(a, b *declaration) bool {
	return a != nil && b != nil && a.ref == b.ref && bytes.Equal(a.descriptor, b.descriptor) && a.valueType == b.valueType && a.metadataType == b.metadataType
}
