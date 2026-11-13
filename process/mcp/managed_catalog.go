package mcp

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type managedMethodRequirement struct {
	identity value.JSON
	export   string
}

// Managed exports share one method namespace in operations.describe.
// The frozen catalog must be satisfiable before any backend can execute.
type managedCatalog struct {
	exports map[string]*ManagedContract
	methods map[string]managedMethodRequirement
}

func (c *managedCatalog) admit(export, physical string, contract *ManagedContract) error {
	if prior := c.exports[physical]; prior != nil {
		if prior.wire != contract.wire {
			return fmt.Errorf("%w: managed export %q has conflicting contracts", frame.ErrContractMismatch, physical)
		}
		return nil
	}
	methods := make(map[string]managedMethodRequirement)
	for _, descriptor := range contract.capability.Descriptor().Methods {
		method, _ := contract.capability.Method(descriptor.Name)
		required := managedMethodRequirement{identity: method.Identity(), export: export}
		if prior, exists := c.methods[method.Name()]; exists && prior.identity != required.identity {
			return fmt.Errorf("%w: managed method %q contracts conflict between exports %q and %q", frame.ErrContractMismatch, method.Name(), prior.export, export)
		}
		methods[method.Name()] = required
	}
	if c.exports == nil {
		c.exports = make(map[string]*ManagedContract)
		c.methods = make(map[string]managedMethodRequirement)
	}
	c.exports[physical] = contract
	for name, required := range methods {
		if _, exists := c.methods[name]; !exists {
			c.methods[name] = required
		}
	}
	return nil
}

type managedDescription struct {
	Protocol     string                   `json:"protocol"`
	Methods      []frame.MethodDescriptor `json:"methods"`
	Capabilities []struct {
		Name     string         `json:"name"`
		Contract jsontext.Value `json:"contract"`
	} `json:"capabilities"`
}

func (c *managedCatalog) verify(description managedDescription) (*frame.Method, error) {
	if description.Protocol != managedVersion || len(description.Capabilities) != len(c.exports) {
		return nil, fmt.Errorf("%w: managed description differs from admission", frame.ErrContractMismatch)
	}
	methods := make(map[string]*frame.Method, len(description.Methods))
	for _, descriptor := range description.Methods {
		if methods[descriptor.Name] != nil || descriptor.Target == "" {
			return nil, frame.ErrContractMismatch
		}
		method, err := frame.AdmitMethod(descriptor)
		if err != nil {
			return nil, err
		}
		methods[method.Name()] = method
	}
	seen := make(map[string]bool, len(c.exports))
	for _, entry := range description.Capabilities {
		required := c.exports[entry.Name]
		if required == nil || seen[entry.Name] {
			return nil, fmt.Errorf("%w: unexpected managed export %q", frame.ErrContractMismatch, entry.Name)
		}
		seen[entry.Name] = true
		actual, err := ParseManagedContract(entry.Contract)
		if err != nil || actual.wire != required.wire {
			return nil, errors.Join(frame.ErrContractMismatch, err)
		}
	}
	for name, required := range c.methods {
		if published := methods[name]; published == nil || published.Identity() != required.identity {
			return nil, fmt.Errorf("%w: inconsistent managed method %q", frame.ErrContractMismatch, name)
		}
	}
	receipt := methods[managedReceipt]
	if receipt != nil && receipt.Identity().Digest().String() != receiptFingerprint {
		return nil, fmt.Errorf("%w: unexpected managed receipt contract", frame.ErrContractMismatch)
	}
	return receipt, nil
}
