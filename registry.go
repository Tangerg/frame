package frame

import (
	"bytes"

	"github.com/Tangerg/frame/internal/nilness"
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
			return ErrContractMismatch
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

func (h *Host) Register(declarations ...Declaration) error {
	definitions := make([]*declaration, 0, len(declarations))
	for _, token := range declarations {
		if nilness.Is(token) || token.definition() == nil {
			return ErrInvalid
		}
		definitions = append(definitions, token.definition())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() {
		return ErrClosed
	}
	return h.contracts.admit(definitions)
}

// RegisterDescriptor admits a declaration whose native implementation lives in
// another execution realm. That realm validates its local language value type.
func (h *Host) RegisterDescriptor(ref ContractRef, descriptor []byte) error {
	definition, err := descriptorDeclaration(ref, descriptor)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() {
		return ErrClosed
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
		return nil, ErrContractMismatch
	}
	return &declaration{contractDefinition: &contractDefinition{ref: ref, descriptor: bytes.Clone(descriptor)}}, nil
}

// ValueContract identifies the callable contract behind a reference: a service
// is its own value contract, a portable Point names its member contract.
func (h *Host) ValueContract(ref ContractRef) (ContractRef, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	definition := h.contracts.lookup(ref)
	switch {
	case definition == nil:
		return ContractRef{}, ErrContractMismatch
	case ref.Kind == ServiceKind:
		return ref, nil
	case ref.Kind != PointKind || definition.valueContract == (ContractRef{}) || h.contracts.lookup(definition.valueContract) == nil:
		return ContractRef{}, ErrContractMismatch
	default:
		return definition.valueContract, nil
	}
}

func (h *Host) ContractDescriptor(ref ContractRef) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	definition := h.contracts.lookup(ref)
	if definition == nil {
		return nil, ErrContractMismatch
	}
	return bytes.Clone(definition.descriptor), nil
}

func (h *Host) DecodeMetadata(contract ContractRef, data []byte) (any, error) {
	value, err := framevalue.ParseJSON(data)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	definition := h.contracts.lookup(contract)
	types := h.contracts.goTypes[contract]
	h.mu.Unlock()
	if definition == nil {
		return nil, ErrContractMismatch
	}
	return decodeMetadata(definition, types.metadata, value)
}
