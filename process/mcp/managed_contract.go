package mcp

import (
	"fmt"
	"regexp"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

const managedNamespace = "io.github.tangerg.qimu/"
const managedVersion = managedNamespace + "portable/1"

var managedExportName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

type managedComposition string

const (
	managedCollection managedComposition = "collection"
	managedSelection  managedComposition = "selection"
	managedDecoration managedComposition = "decoration"
)

type managedDescriptor struct {
	Protocol    string                   `json:"protocol"`
	Reference   string                   `json:"reference"`
	Composition managedComposition       `json:"composition"`
	Scope       string                   `json:"scope"`
	Methods     []frame.MethodDescriptor `json:"methods"`
}

// ManagedContract projects both identities from the one descriptor shipped by
// the contract owner. Qimu includes composition and scope in its wire identity.
type ManagedContract struct {
	capability *frame.Capability
	wire       value.JSON
}

func ParseManagedContract(data []byte) (*ManagedContract, error) {
	if err := value.CheckDescriptor(data); err != nil {
		return nil, err
	}
	var descriptor managedDescriptor
	if err := value.DecodeValue(data, &descriptor, "managed capability"); err != nil {
		return nil, err
	}
	if descriptor.Protocol != managedVersion || !frame.ValidName(descriptor.Scope) {
		return nil, frame.ErrContractMismatch
	}
	switch descriptor.Composition {
	case managedCollection, managedSelection, managedDecoration:
	default:
		return nil, frame.ErrContractMismatch
	}
	for _, member := range descriptor.Methods {
		if member.Target == "" {
			return nil, fmt.Errorf("%w: missing managed method target", frame.ErrContractMismatch)
		}
	}
	capability, err := frame.NewCapability(descriptor.Reference, descriptor.Methods...)
	if err != nil {
		return nil, err
	}
	descriptor.Methods = capability.Descriptor().Methods
	for i := range descriptor.Methods {
		descriptor.Methods[i].Description = ""
	}
	identity, err := value.EncodeJSON(descriptor)
	if err != nil {
		return nil, err
	}
	return &ManagedContract{capability: capability, wire: identity}, nil
}

func (c *ManagedContract) Capability() *frame.Capability {
	if c.capability == nil {
		return nil
	}
	return new(*c.capability)
}
