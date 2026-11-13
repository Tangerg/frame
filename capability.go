package frame

import (
	"errors"
	"slices"
	"strings"

	"github.com/Tangerg/frame/value"
)

const MaxCapabilityMethods = 256

type CapabilityDescriptor struct {
	Protocol  string             `json:"protocol"`
	Reference string             `json:"reference"`
	Methods   []MethodDescriptor `json:"methods"`
}

// Producer identity, execution location and availability cannot change the
// consumer's required semantics; those facts belong to the concrete binding.
type Capability struct {
	reference string
	methods   []*Method
	identity  value.JSON
}

func NewCapability(reference string, methods ...MethodDescriptor) (*Capability, error) {
	return AdmitCapability(CapabilityDescriptor{Protocol: CapabilityProtocol, Reference: reference, Methods: methods})
}

func ParseCapability(data []byte) (*Capability, error) {
	if err := value.CheckDescriptor(data); err != nil {
		return nil, err
	}
	var descriptor CapabilityDescriptor
	if err := value.DecodeValue(data, &descriptor, "capability"); err != nil {
		return nil, err
	}
	return AdmitCapability(descriptor)
}

func AdmitCapability(descriptor CapabilityDescriptor) (*Capability, error) {
	if descriptor.Protocol != CapabilityProtocol {
		return nil, errors.New("capability: current protocol is required")
	}
	if !ValidName(descriptor.Reference) {
		return nil, errors.New("capability: bounded reference is required")
	}
	if len(descriptor.Methods) == 0 {
		return nil, errors.New("capability: required methods are missing")
	}
	if len(descriptor.Methods) > MaxCapabilityMethods {
		return nil, value.NewLimitError("capability methods", MaxCapabilityMethods)
	}
	for _, method := range descriptor.Methods {
		if err := checkMethodDescriptor(method); err != nil {
			return nil, err
		}
	}
	if err := value.CheckDescriptorValue(descriptor); err != nil {
		return nil, err
	}
	contract := &Capability{reference: descriptor.Reference}
	for _, descriptor := range descriptor.Methods {
		method, err := AdmitMethod(descriptor)
		if err != nil {
			return nil, err
		}
		contract.methods = append(contract.methods, method)
	}
	slices.SortFunc(contract.methods, func(a, b *Method) int { return strings.Compare(a.Name(), b.Name()) })
	for i := 1; i < len(contract.methods); i++ {
		if contract.methods[i-1].Name() == contract.methods[i].Name() {
			return nil, errors.New("capability: duplicate required method")
		}
	}
	canonical, err := value.EncodeJSON(contract.IdentityModel().Descriptor())
	if err != nil {
		return nil, err
	}
	contract.identity = canonical
	return contract, nil
}

func (c *Capability) IdentityModel() *Capability {
	model := new(*c)
	model.methods = make([]*Method, len(c.methods))
	for i, method := range c.methods {
		model.methods[i] = method.identityModel()
	}
	return model
}

func (c *Capability) Descriptor() CapabilityDescriptor {
	descriptor := CapabilityDescriptor{Protocol: CapabilityProtocol, Reference: c.reference, Methods: make([]MethodDescriptor, len(c.methods))}
	for i, method := range c.methods {
		descriptor.Methods[i] = method.Descriptor()
	}
	return descriptor
}

func (c *Capability) Identity() value.JSON { return c.identity }

// Replacing a lookup result cannot change the validation behind the published identity.
func (c *Capability) Method(name string) (*Method, bool) {
	for _, method := range c.methods {
		if method.Name() == name {
			return new(*method), true
		}
	}
	return nil, false
}
