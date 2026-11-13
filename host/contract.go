package host

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"reflect"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
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

func (ContractKind) JSONSchemaEnum() []string {
	return []string{string(ServiceKind), string(PointKind), string(EventKind)}
}
func (ContractMode) JSONSchemaEnum() []string {
	return []string{string(NativeMode), string(PortableMode)}
}

// ContractRef is a transport and persistence projection of a declaration. Its
// digest is derived from the original descriptor, not from implementation types.
type ContractRef struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Digest  framevalue.Digest `json:"digest"`
	Kind    ContractKind      `json:"kind"`
	Mode    ContractMode      `json:"mode"`
}

func (r ContractRef) Validate() error {
	if err := validateName("contract name", r.Name); err != nil {
		return err
	}
	if err := validateName("contract version", r.Version); err != nil {
		return err
	}
	if err := r.Digest.Validate(); err != nil {
		return fmt.Errorf("%w: contract digest: %w", frame.ErrInvalid, err)
	}
	if !slices.Contains(r.Kind.JSONSchemaEnum(), string(r.Kind)) {
		return fmt.Errorf("%w: contract kind", frame.ErrInvalid)
	}
	if !slices.Contains(r.Mode.JSONSchemaEnum(), string(r.Mode)) {
		return fmt.Errorf("%w: contract mode", frame.ErrInvalid)
	}
	return nil
}

func (r ContractRef) String() string { return r.Name + "@" + r.Version }

// ContractKey groups declarations that must agree on one exact ContractRef.
// Native and portable implementations cannot independently claim that name.
type ContractKey struct {
	Kind    ContractKind
	Name    string
	Version string
}

func (r ContractRef) Key() ContractKey {
	return ContractKey{Kind: r.Kind, Name: r.Name, Version: r.Version}
}

type contractDefinition struct {
	ref            ContractRef
	descriptor     []byte
	valueContract  ContractRef
	metadataSchema *framevalue.Schema
	capability     *frame.Capability
}

type goContractTypes struct {
	value    reflect.Type
	metadata reflect.Type
}

type declaration struct {
	*contractDefinition
	goTypes goContractTypes
}

// Declaration can only be produced by the typed constructors. Ref values
// received over a wire still require their original admitted declaration.
type Declaration interface {
	Ref() ContractRef
	Descriptor() []byte
	definition() *declaration
}

type Contract[T any] struct{ value *declaration }
type Point[M, V any] struct{ value *declaration }
type Event[T any] struct{ value *declaration }

func (c Contract[T]) Ref() ContractRef { return c.value.reference() }
func (p Point[M, V]) Ref() ContractRef { return p.value.reference() }
func (e Event[T]) Ref() ContractRef    { return e.value.reference() }

// reference makes the zero value of a typed declaration name no contract.
func (d *declaration) reference() ContractRef {
	if d == nil {
		return ContractRef{}
	}
	return d.ref
}
func (c Contract[T]) definition() *declaration { return c.value }
func (p Point[M, V]) definition() *declaration { return p.value }
func (e Event[T]) definition() *declaration    { return e.value }

// Descriptor returns the original immutable identity document. Local Go types
// and driver proxy types are never included in its wire identity.
func (c Contract[T]) Descriptor() []byte { return c.value.descriptorCopy() }
func (p Point[M, V]) Descriptor() []byte { return p.value.descriptorCopy() }
func (e Event[T]) Descriptor() []byte    { return e.value.descriptorCopy() }

func (d *declaration) descriptorCopy() []byte {
	if d == nil {
		return nil
	}
	return bytes.Clone(d.descriptor)
}

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
	if err := framevalue.CheckDescriptor(descriptor); err != nil {
		return nil, err
	}
	if ref.Kind != PointKind || ref.Mode != PortableMode || framevalue.Hash(descriptor) != ref.Digest {
		return nil, frame.ErrContractMismatch
	}
	var shape PointDescriptor
	if err := framevalue.DecodeValue(descriptor, &shape, "point"); err != nil {
		return nil, err
	}
	if shape.Protocol != PointProtocol || shape.Name != ref.Name || shape.Version != ref.Version {
		return nil, frame.ErrContractMismatch
	}
	if err := shape.ValueContract.Validate(); err != nil {
		return nil, err
	}
	if shape.ValueContract.Kind != ServiceKind || shape.ValueContract.Mode != PortableMode {
		return nil, frame.ErrContractMismatch
	}
	schema, err := framevalue.AdmitSchema(shape.Metadata)
	if err != nil {
		return nil, fmt.Errorf("point metadata: %w", err)
	}
	return &declaration{contractDefinition: &contractDefinition{ref: ref, descriptor: bytes.Clone(descriptor), valueContract: shape.ValueContract, metadataSchema: &schema}}, nil
}

func NewPortablePoint[M, V any](name, version string, metadataSchema []byte, value Contract[V]) (Point[M, V], error) {
	if value.value == nil || value.Ref().Mode != PortableMode {
		return Point[M, V]{}, frame.ErrContractMismatch
	}
	if err := validateName("contract name", name); err != nil {
		return Point[M, V]{}, err
	}
	if err := validateName("contract version", version); err != nil {
		return Point[M, V]{}, err
	}
	if err := framevalue.CheckDescriptor(metadataSchema); err != nil {
		return Point[M, V]{}, err
	}
	shape := PointDescriptor{Protocol: PointProtocol, Name: name, Version: version, Metadata: bytes.Clone(metadataSchema), ValueContract: value.Ref()}
	identity, err := framevalue.EncodeJSON(shape)
	if err != nil {
		return Point[M, V]{}, err
	}
	ref := ContractRef{Name: name, Version: version, Digest: identity.Digest(), Kind: PointKind, Mode: PortableMode}
	return BindPoint[M, V](ref, identity.Bytes())
}

// nativeDescriptor is the identity document of a native contract. Language
// types are deliberately absent: they never change a contract's identity.
type nativeDescriptor struct {
	Name    string       `json:"name"`
	Version string       `json:"version"`
	Kind    ContractKind `json:"kind"`
	Mode    ContractMode `json:"mode"`
}

func nativeDeclaration(name, version string, kind ContractKind, valueType, metadataType reflect.Type) (*declaration, error) {
	descriptor, err := json.Marshal(nativeDescriptor{Name: name, Version: version, Kind: kind, Mode: NativeMode})
	if err != nil {
		return nil, err
	}
	ref := ContractRef{Name: name, Version: version, Digest: framevalue.Hash(descriptor), Kind: kind, Mode: NativeMode}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	return &declaration{contractDefinition: &contractDefinition{ref: ref, descriptor: descriptor}, goTypes: goContractTypes{value: valueType, metadata: metadataType}}, nil
}

func NewContract[T any](name, version string) (Contract[T], error) {
	value, err := nativeDeclaration(name, version, ServiceKind, reflect.TypeFor[T](), nil)
	return Contract[T]{value: value}, err
}

// NewPortableContract binds a published capability to its local implementation type.
// The owner supplies the contract version. Name and content identity come from the capability.
func NewPortableContract[T any](version string, capability *frame.Capability) (Contract[T], error) {
	if capability == nil || capability.Identity().Validate() != nil {
		return Contract[T]{}, frame.ErrInvalid
	}
	captured := capability.IdentityModel()
	identity := captured.Identity()
	ref := ContractRef{Name: captured.Descriptor().Reference, Version: version, Kind: ServiceKind, Mode: PortableMode, Digest: identity.Digest()}
	if err := ref.Validate(); err != nil {
		return Contract[T]{}, err
	}
	return Contract[T]{value: &declaration{contractDefinition: &contractDefinition{ref: ref, descriptor: identity.Bytes(), capability: captured}, goTypes: goContractTypes{value: reflect.TypeFor[T]()}}}, nil
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
	if ref.Kind != ServiceKind || ref.Mode != PortableMode || framevalue.Hash(descriptor) != ref.Digest {
		return Contract[T]{}, frame.ErrContractMismatch
	}
	definition, err := descriptorDeclaration(ref, descriptor)
	if err != nil {
		return Contract[T]{}, err
	}
	definition.goTypes.value = reflect.TypeFor[T]()
	return Contract[T]{value: definition}, nil
}

func BindPoint[M, V any](ref ContractRef, descriptor []byte) (Point[M, V], error) {
	metadataType := reflect.TypeFor[M]()
	if err := framevalue.ValidateType(metadataType); err != nil {
		return Point[M, V]{}, fmt.Errorf("%w: point metadata projection: %w", frame.ErrContractMismatch, err)
	}
	value, err := pointDeclaration(ref, descriptor)
	if err != nil {
		return Point[M, V]{}, err
	}
	value.goTypes = goContractTypes{value: reflect.TypeFor[V](), metadata: metadataType}
	return Point[M, V]{value: value}, nil
}
