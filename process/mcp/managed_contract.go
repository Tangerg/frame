package mcp

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"regexp"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

const managedNamespace = "io.github.tangerg.qimu/"
const managedVersion = managedNamespace + "portable/1"

var managedExportName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

type managedDescriptor struct {
	Protocol    string                      `json:"protocol"`
	Reference   string                      `json:"reference"`
	Composition string                      `json:"composition"`
	Scope       string                      `json:"scope"`
	Methods     []portable.MethodDescriptor `json:"methods"`
}

// ManagedContract projects both identities from the one descriptor shipped by
// the contract owner. Qimu includes composition and scope in its wire identity.
type ManagedContract struct {
	capability *portable.Capability
	wire       value.JSON
}

func ParseManagedContract(data []byte) (*ManagedContract, error) {
	if err := portable.CheckDescriptor(data); err != nil {
		return nil, err
	}
	var descriptor managedDescriptor
	if err := value.DecodeValue(data, &descriptor, "managed capability"); err != nil {
		return nil, err
	}
	if descriptor.Protocol != managedVersion || !portable.ValidToken(descriptor.Scope) {
		return nil, frame.ErrContractMismatch
	}
	switch descriptor.Composition {
	case "collection", "selection", "decoration":
	default:
		return nil, frame.ErrContractMismatch
	}
	for _, member := range descriptor.Methods {
		if member.Target == "" {
			return nil, fmt.Errorf("%w: missing managed method target", frame.ErrContractMismatch)
		}
	}
	capability, err := portable.NewCapability(descriptor.Reference, descriptor.Methods...)
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

func (c *ManagedContract) Capability() *portable.Capability { return c.capability }

type managedDescription struct {
	Protocol     string                      `json:"protocol"`
	Methods      []portable.MethodDescriptor `json:"methods"`
	Capabilities []struct {
		Name     string         `json:"name"`
		Contract jsontext.Value `json:"contract"`
	} `json:"capabilities"`
}

func (d managedDescription) verify(expected map[string]*ManagedContract) (*portable.Method, error) {
	if d.Protocol != managedVersion || len(d.Capabilities) != len(expected) {
		return nil, fmt.Errorf("%w: managed description differs from admission", frame.ErrContractMismatch)
	}
	methods := make(map[string]*portable.Method, len(d.Methods))
	for _, descriptor := range d.Methods {
		if methods[descriptor.Name] != nil || descriptor.Target == "" {
			return nil, frame.ErrContractMismatch
		}
		method, err := portable.AdmitMethod(descriptor)
		if err != nil {
			return nil, err
		}
		methods[method.Name()] = method
	}
	seen := make(map[string]bool, len(expected))
	for _, entry := range d.Capabilities {
		required := expected[entry.Name]
		if required == nil || seen[entry.Name] {
			return nil, fmt.Errorf("%w: unexpected managed export %q", frame.ErrContractMismatch, entry.Name)
		}
		seen[entry.Name] = true
		actual, err := ParseManagedContract(entry.Contract)
		if err != nil || actual.wire != required.wire {
			return nil, errors.Join(frame.ErrContractMismatch, err)
		}
		for _, member := range required.capability.Descriptor().Methods {
			published := methods[member.Name]
			requiredMethod, _ := required.capability.Method(member.Name)
			if published == nil || published.Identity() != requiredMethod.Identity() {
				return nil, fmt.Errorf("%w: inconsistent managed method %q", frame.ErrContractMismatch, member.Name)
			}
		}
	}
	receipt := methods[managedReceipt]
	if receipt != nil && receipt.Identity().Digest().String() != receiptFingerprint {
		return nil, fmt.Errorf("%w: unexpected managed receipt contract", frame.ErrContractMismatch)
	}
	return receipt, nil
}
