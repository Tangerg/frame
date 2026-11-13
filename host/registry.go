package host

import (
	"bytes"
	json "encoding/json/v2"
	"errors"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// Contract identity is global; Go types describe only this Host's local view.
// Descriptor registration cannot replace or suppress that separate binding.
// Host.mu guards both admissions.
type contractRegistry struct {
	declarations map[ContractRef]*contractDefinition
	digests      map[ContractKey]ContractRef
	goTypes      map[ContractRef]goContractTypes
}

func newContractRegistry() contractRegistry {
	return contractRegistry{declarations: make(map[ContractRef]*contractDefinition), digests: make(map[ContractKey]ContractRef), goTypes: make(map[ContractRef]goContractTypes)}
}

func (r *contractRegistry) lookup(ref ContractRef) *contractDefinition { return r.declarations[ref] }

func (r *contractRegistry) admit(definitions []*declaration) error {
	batch := newContractRegistry()
	for _, definition := range definitions {
		if r.conflicts(definition) || batch.conflicts(definition) {
			return frame.ErrContractMismatch
		}
		batch.add(definition)
	}
	for _, definition := range definitions {
		r.add(definition)
	}
	return nil
}

func (r *contractRegistry) conflicts(definition *declaration) bool {
	if ref, exists := r.digests[definition.ref.Key()]; exists && ref != definition.ref {
		return true
	}
	if admitted := r.lookup(definition.ref); admitted != nil && !bytes.Equal(admitted.descriptor, definition.descriptor) {
		return true
	}
	local, bound := r.goTypes[definition.ref]
	return bound && definition.goTypes.value != nil && local != definition.goTypes
}

func (r *contractRegistry) add(definition *declaration) {
	if r.lookup(definition.ref) == nil {
		r.declarations[definition.ref] = definition.contractDefinition
		r.digests[definition.ref.Key()] = definition.ref
	}
	if definition.goTypes.value != nil {
		r.goTypes[definition.ref] = definition.goTypes
	}
}

func (r *contractRegistry) matches(definition *declaration) bool {
	admitted := r.lookup(definition.ref)
	return admitted != nil && bytes.Equal(admitted.descriptor, definition.descriptor) && r.goTypes[definition.ref] == definition.goTypes
}

func (h *hostState) Register(declarations ...Declaration) error {
	definitions := make([]*declaration, 0, len(declarations))
	for _, token := range declarations {
		if framevalue.IsNil(token) || token.definition() == nil {
			return frame.ErrInvalid
		}
		definitions = append(definitions, token.definition())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.IsClosing() {
		return frame.ErrClosed
	}
	return h.contracts.admit(definitions)
}

// RegisterDescriptor admits a declaration whose native implementation lives in
// another execution realm. That realm validates its local language value type.
func (h *hostState) RegisterDescriptor(ref ContractRef, descriptor []byte) error {
	definition, err := descriptorDeclaration(ref, descriptor)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.IsClosing() {
		return frame.ErrClosed
	}
	return h.contracts.admit([]*declaration{definition})
}

func descriptorDeclaration(ref ContractRef, descriptor []byte) (*declaration, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if ref.Kind == PointKind && ref.Mode == PortableMode {
		return pointDeclaration(ref, descriptor)
	}
	if framevalue.Hash(descriptor) != ref.Digest {
		return nil, frame.ErrContractMismatch
	}
	if err := framevalue.CheckDescriptor(descriptor); err != nil {
		return nil, errors.Join(frame.ErrContractMismatch, err)
	}
	definition := &declaration{contractDefinition: &contractDefinition{ref: ref, descriptor: bytes.Clone(descriptor)}}
	if ref.Kind != ServiceKind || ref.Mode != PortableMode || bytes.TrimSpace(descriptor)[0] != '{' {
		return definition, nil
	}
	var header struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(descriptor, &header); err != nil {
		return nil, errors.Join(frame.ErrContractMismatch, err)
	}
	if header.Protocol != frame.CapabilityProtocol {
		return definition, nil
	}
	capability, err := frame.ParseCapability(descriptor)
	if err != nil {
		return nil, errors.Join(frame.ErrContractMismatch, err)
	}
	if capability.Descriptor().Reference != ref.Name || capability.Identity().Digest() != ref.Digest {
		return nil, frame.ErrContractMismatch
	}
	definition.capability = capability
	return definition, nil
}

func (r *contractRegistry) valueDefinition(ref ContractRef) *contractDefinition {
	definition := r.lookup(ref)
	if definition != nil && ref.Kind == PointKind {
		definition = r.lookup(definition.valueContract)
	}
	if definition == nil || definition.ref.Kind != ServiceKind {
		return nil
	}
	return definition
}

// ValueContract identifies the callable contract behind a reference: a service
// is its own value contract, a portable Point names its member contract.
func (h *hostState) ValueContract(ref ContractRef) (ContractRef, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	definition := h.contracts.valueDefinition(ref)
	if definition == nil {
		return ContractRef{}, frame.ErrContractMismatch
	}
	return definition.ref, nil
}

// The returned handle cannot replace the admitted model retained by this Host.
func (h *hostState) Capability(ref ContractRef) (*frame.Capability, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	definition := h.contracts.valueDefinition(ref)
	if definition == nil || definition.capability == nil {
		return nil, frame.ErrContractMismatch
	}
	return new(*definition.capability), nil
}

func (h *hostState) ContractDescriptor(ref ContractRef) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	definition := h.contracts.lookup(ref)
	if definition == nil {
		return nil, frame.ErrContractMismatch
	}
	return bytes.Clone(definition.descriptor), nil
}

func (h *hostState) PrepareMetadata(contract ContractRef, data []byte) (PreparedMetadata, error) {
	value, err := framevalue.ParseJSON(data)
	if err != nil {
		return PreparedMetadata{}, err
	}
	h.mu.Lock()
	definition := h.contracts.lookup(contract)
	types := h.contracts.goTypes[contract]
	h.mu.Unlock()
	if definition == nil {
		return PreparedMetadata{}, frame.ErrContractMismatch
	}
	if err := validateMetadata(definition, types.metadata, value); err != nil {
		return PreparedMetadata{}, err
	}
	return PreparedMetadata{contract: contract, projectionType: types.metadata, value: value}, nil
}
