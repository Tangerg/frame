package frame

import (
	"bytes"

	"github.com/Tangerg/frame/internal/nilness"
	framevalue "github.com/Tangerg/frame/value"
)

// contractRegistry admits contract declarations. Within one Host a kind, name
// and version bind exactly one digest; Host.mu guards it.
type contractRegistry struct {
	declarations map[ContractRef]*declaration
	digests      map[ContractKey]ContractRef
}

func newContractRegistry() contractRegistry {
	return contractRegistry{declarations: make(map[ContractRef]*declaration), digests: make(map[ContractKey]ContractRef)}
}

func (r *contractRegistry) lookup(ref ContractRef) *declaration { return r.declarations[ref] }

// admit registers definitions atomically. same decides whether a definition
// repeats an already admitted declaration of the same reference.
func (r *contractRegistry) admit(definitions []*declaration, same func(admitted, next *declaration) bool) error {
	batch := contractRegistry{declarations: make(map[ContractRef]*declaration, len(definitions)), digests: make(map[ContractKey]ContractRef, len(definitions))}
	for _, definition := range definitions {
		if r.conflicts(definition, same) || batch.conflicts(definition, same) {
			return ErrContractMismatch
		}
		batch.add(definition)
	}
	for _, definition := range batch.declarations {
		if r.declarations[definition.ref] == nil {
			r.add(definition)
		}
	}
	return nil
}

func (r *contractRegistry) conflicts(definition *declaration, same func(admitted, next *declaration) bool) bool {
	if ref, exists := r.digests[definition.ref.Key()]; exists && ref != definition.ref {
		return true
	}
	admitted := r.declarations[definition.ref]
	return admitted != nil && !same(admitted, definition)
}

func (r *contractRegistry) add(definition *declaration) {
	r.declarations[definition.ref] = definition
	r.digests[definition.ref.Key()] = definition.ref
}

func sameDescriptor(admitted, next *declaration) bool {
	return bytes.Equal(admitted.descriptor, next.descriptor)
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
	return h.contracts.admit(definitions, sameDeclaration)
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
	return h.contracts.admit([]*declaration{definition}, sameDescriptor)
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
	return &declaration{ref: ref, descriptor: bytes.Clone(descriptor)}, nil
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
	h.mu.Unlock()
	if definition == nil {
		return nil, ErrContractMismatch
	}
	return decodeMetadata(definition, value)
}
