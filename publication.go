package frame

import (
	"context"
	"crypto/rand"
	"fmt"
	"reflect"
	"slices"

	"github.com/Tangerg/frame/internal/nilness"
)

type Publication struct {
	ID         string
	Source     ExportRef
	InstanceID string
	Revision   uint64
}

func (p *publication) view() Publication {
	return Publication{ID: p.id, Source: p.source, InstanceID: p.instance.id, Revision: p.revision}
}

func (i *unitInstance) exportRef(name string, contract ContractRef) ExportRef {
	source := i.binding.Source
	return ExportRef{InstallationID: source.InstallationID, Release: source.Release, UnitID: source.UnitID, Export: name, Contract: contract}
}

// declaresPoint permits dynamic membership in a Point the unit declared. A
// service export cannot be added or replaced after setup.
func declaresPoint(unit UnitDeclaration, contract ContractRef) bool {
	return contract.Kind == PointKind && slices.ContainsFunc(unit.Exports, func(export ExportDeclaration) bool { return export.Contract == contract })
}

// setupPublications checks a setup's actual exports against its declaration:
// each declared export exactly once, plus dynamic members of declared Points.
func (h *Host) setupPublications(ctx context.Context, instance *unitInstance, exports []ExportValue) ([]*publication, error) {
	actual := make(map[string]bool, len(exports))
	result := make([]*publication, 0, len(exports))
	for _, export := range exports {
		if err := validateName("export name", export.Name); err != nil {
			return nil, err
		}
		if actual[export.Name] {
			return nil, fmt.Errorf("%w: duplicate actual export", ErrInvalid)
		}
		actual[export.Name] = true
		declared, found := declaredExport(instance.unit, export.Name)
		if found && declared.Contract != export.Contract || !found && !declaresPoint(instance.unit, export.Contract) {
			return nil, fmt.Errorf("%w: actual export was not declared", ErrContractMismatch)
		}
		value, err := h.prepareExport(ctx, instance, export)
		if err != nil {
			return nil, err
		}
		value.durable = found
		result = append(result, value)
	}
	for _, declared := range instance.unit.Exports {
		if !actual[declared.Name] {
			return nil, fmt.Errorf("%w: declared export %s missing", ErrContractMismatch, declared.Name)
		}
	}
	return result, nil
}

func declaredExport(unit UnitDeclaration, name string) (ExportDeclaration, bool) {
	for _, export := range unit.Exports {
		if export.Name == name {
			return export, true
		}
	}
	return ExportDeclaration{}, false
}

// prepareExport admits one actual value and a detached metadata snapshot
// without holding Host.mu, because metadata decoding may be slow.
func (h *Host) prepareExport(ctx context.Context, instance *unitInstance, export ExportValue) (*publication, error) {
	h.mu.Lock()
	definition := h.contracts.lookup(export.Contract)
	types := h.contracts.goTypes[export.Contract]
	driver := h.drivers[instance.unit.Profile]
	h.mu.Unlock()
	if definition == nil {
		return nil, ErrContractMismatch
	}
	if validator, ok := driver.Driver.(ExportValidator); ok {
		if err := instance.validateCurrent(ctx); err != nil {
			return nil, err
		}
		if err := validator.ValidateExport(instance.id, export); err != nil {
			return nil, err
		}
	} else if export.Contract.Kind != EventKind {
		if nilness.Is(export.Value) {
			return nil, fmt.Errorf("%w: nil actual export", ErrContractMismatch)
		}
		if types.value == nil {
			return nil, fmt.Errorf("%w: export has no local type binding", ErrContractMismatch)
		}
		if !reflect.TypeOf(export.Value).AssignableTo(types.value) {
			return nil, ErrContractMismatch
		}
	}
	if err := instance.validateCurrent(ctx); err != nil {
		return nil, err
	}
	metadata, err := prepareMetadata(definition, types.metadata, export.Metadata)
	if err != nil {
		return nil, err
	}
	return &publication{id: rand.Text(), source: instance.exportRef(export.Name, export.Contract), metadata: metadata, value: export.Value, instance: instance}, nil
}

// PublicationRequest fixes one native implementation and metadata snapshot. A
// replay must retain this request because arbitrary Go values have no structural
// identity that can safely distinguish implementations, closures or pointers.
type PublicationRequest struct {
	owner    *authorityIdentity
	identity publicationRequestID
	instance *unitInstance
	source   ExportRef
	metadata any
	value    any
}

type publicationRequestID string

func (h *Host) PreparePublication(ctx context.Context, instanceID string, value ExportValue) (*PublicationRequest, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	instance := h.allInstances[instanceID]
	err := ErrUnavailable
	var work *Work
	if instance != nil {
		err = instance.validateServingLocked()
		if err == nil {
			work, err = instance.lifetime.Enter(ctx)
		}
	}
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	defer work.Finish()
	if !declaresPoint(instance.unit, value.Contract) {
		return nil, ErrContractMismatch
	}
	if err := validateName("export name", value.Name); err != nil {
		return nil, err
	}
	prepared, err := h.prepareExport(work.Context(), instance, value)
	if err != nil {
		return nil, err
	}
	if err := instance.validateCurrent(work.Context()); err != nil {
		return nil, err
	}
	return &PublicationRequest{owner: h.identity, identity: publicationRequestID(prepared.id), instance: instance, source: prepared.source, metadata: prepared.metadata, value: prepared.value}, nil
}

// A returned error proves this call did not publish. An earlier successful
// operation remains authoritative and can only be replayed with its request.
func (h *Host) Publish(ctx context.Context, operationID string, request *PublicationRequest) (Publication, error) {
	if request == nil || request.owner != h.identity || request.identity == "" {
		return Publication{}, ErrInvalid
	}
	if err := validateName("publication operation", operationID); err != nil {
		return Publication{}, err
	}
	var result Publication
	err := h.admission.Admit(ctx, func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		instance := request.instance
		if h.allInstances[instance.id] != instance {
			return ErrUnavailable
		}
		if err := instance.validateServingLocked(); err != nil {
			return err
		}
		value, err := instance.publishLocked(operationID, request)
		if err != nil {
			return err
		}
		result = value.view()
		return nil
	})
	return result, err
}

func (i *unitInstance) publishLocked(operationID string, request *PublicationRequest) (*publication, error) {
	if original := i.publicationOperations[operationID]; original != nil {
		if original.request != request.identity || !original.publication.visible {
			return nil, ErrStaleBinding
		}
		return original.publication, nil
	}
	if len(i.publicationOperations) >= i.host.limits.MaxPublicationOperations {
		return nil, fmt.Errorf("%w: publication operations", ErrCapacity)
	}
	if err := i.host.catalog.reserve(1); err != nil {
		return nil, err
	}
	if i.publishesLocked(request.source.Export) {
		return nil, fmt.Errorf("%w: export is already published", ErrInvalid)
	}
	value := &publication{id: rand.Text(), source: request.source, metadata: request.metadata, value: request.value, instance: i}
	i.host.catalog.publish(value)
	i.publicationOperations[operationID] = &publicationOperation{request: request.identity, publication: value}
	return value, nil
}

func (i *unitInstance) publishesLocked(name string) bool {
	if export := i.setupExports[name]; export != nil && export.visible {
		return true
	}
	for _, operation := range i.publicationOperations {
		if value := operation.publication; value.source.Export == name && value.visible {
			return true
		}
	}
	return false
}

func (h *Host) Publication(instanceID, operationID string) (Publication, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	instance := h.allInstances[instanceID]
	if instance == nil {
		return Publication{}, ErrUnavailable
	}
	operation := instance.publicationOperations[operationID]
	if operation == nil || !operation.publication.visible {
		return Publication{}, ErrUnavailable
	}
	return operation.publication.view(), nil
}

func (h *Host) Withdraw(ctx context.Context, id string) error {
	return h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		value := h.catalog.find(id)
		if value == nil {
			return ErrUnavailable
		}
		h.catalog.withdraw(value)
		return nil
	})
}
