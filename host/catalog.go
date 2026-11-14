package host

import (
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"reflect"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type preparedExport struct {
	source   ExportRef
	metadata publicationMetadata
	value    any
	instance *unitInstance
}

type publication struct {
	*preparedExport
	id       string
	revision uint64
}

type contractMembers struct {
	revision uint64
	members  map[string]*publication
}

// catalog owns live membership per contract. Every change advances that
// contract's revision and is delivered to its Watches; Host.mu guards it.
type catalog struct {
	contracts       map[ContractRef]*contractMembers
	watches         map[*catalogWatch]struct{}
	maxPublications int
}

func newCatalog(maxPublications int) catalog {
	return catalog{contracts: make(map[ContractRef]*contractMembers), watches: make(map[*catalogWatch]struct{}), maxPublications: maxPublications}
}

func (c *catalog) count() int {
	count := 0
	for _, contract := range c.contracts {
		count += len(contract.members)
	}
	return count
}

func (c *catalog) reserve(publications int) error {
	if c.count()+publications > c.maxPublications {
		return fmt.Errorf("%w: publications", frame.ErrCapacity)
	}
	return nil
}

func (c *catalog) revision(contract ContractRef) uint64 {
	if members := c.contracts[contract]; members != nil {
		return members.revision
	}
	return 0
}

func (c *catalog) member(contract ContractRef, id string) *publication {
	if members := c.contracts[contract]; members != nil {
		return members.members[id]
	}
	return nil
}

func (c *catalog) find(id string) *publication {
	for _, members := range c.contracts {
		if value := members.members[id]; value != nil {
			return value
		}
	}
	return nil
}

func (c *catalog) members(contract ContractRef) []*publication {
	members := c.contracts[contract]
	if members == nil {
		return nil
	}
	result := make([]*publication, 0, len(members.members))
	for _, value := range members.members {
		result = append(result, value)
	}
	slices.SortFunc(result, func(a, b *publication) int {
		return cmp.Or(compareExport(a.source, b.source), cmp.Compare(a.id, b.id))
	})
	return result
}

func (c *catalog) publish(value *publication) {
	members := c.contracts[value.source.Contract]
	if members == nil {
		members = &contractMembers{members: make(map[string]*publication)}
		c.contracts[value.source.Contract] = members
	}
	members.members[value.id] = value
	members.revision++
	value.revision = members.revision
	c.notify(value.source.Contract, members.revision)
}

func (c *catalog) withdraw(value *publication) {
	if !c.contains(value) {
		return
	}
	members := c.contracts[value.source.Contract]
	delete(members.members, value.id)
	members.revision++
	c.notify(value.source.Contract, members.revision)
}

func (c *catalog) contains(value *publication) bool {
	return value != nil && c.member(value.source.Contract, value.id) == value
}

func (c *catalog) notify(contract ContractRef, revision uint64) {
	for watch := range c.watches {
		if slices.Contains(watch.contracts, contract) {
			watch.deliverLocked(Change{Contract: contract, Revision: revision})
		}
	}
}

// unitExports retains original setup slots and publication receipts. Catalog
// membership alone decides visibility; neither index can make a value visible.
type unitExports struct {
	catalog       *catalog
	instance      *unitInstance
	setup         map[string]*publication
	operations    map[string]*publication
	maxOperations int
}

func newUnitExports(catalog *catalog, instance *unitInstance, maxOperations int) *unitExports {
	return &unitExports{
		catalog:       catalog,
		instance:      instance,
		setup:         make(map[string]*publication),
		operations:    make(map[string]*publication),
		maxOperations: maxOperations,
	}
}

func (u *unitExports) bindSetup(values []*publication) error {
	if err := u.catalog.reserve(len(values)); err != nil {
		return err
	}
	for _, value := range values {
		u.setup[value.source.Export] = value
		u.catalog.publish(value)
	}
	return nil
}

func (u *unitExports) reconstructible(value *publication) bool {
	if u.setup[value.source.Export] != value {
		return false
	}
	declared, found := u.instance.unit.export(value.source.Export)
	return found && declared.Contract == value.source.Contract
}

func (u *unitExports) bound(source ExportRef) (*publication, error) {
	value := u.setup[source.Export]
	if value == nil || value.source != source {
		return nil, frame.ErrContractMismatch
	}
	if !u.catalog.contains(value) || !u.reconstructible(value) {
		return nil, ErrUnavailable
	}
	return value, nil
}

func (u *unitExports) publish(operationID string, candidate *publication) (*publication, error) {
	if original := u.operations[operationID]; original != nil {
		if original.preparedExport != candidate.preparedExport || !u.catalog.contains(original) {
			return nil, ErrStaleBinding
		}
		return original, nil
	}
	if len(u.operations) >= u.maxOperations {
		return nil, fmt.Errorf("%w: publication operations", frame.ErrCapacity)
	}
	if err := u.catalog.reserve(1); err != nil {
		return nil, err
	}
	if u.publishes(candidate.source.Export) {
		return nil, fmt.Errorf("%w: export is already published", frame.ErrInvalid)
	}
	u.operations[operationID] = candidate
	u.catalog.publish(candidate)
	return candidate, nil
}

func (u *unitExports) publishes(name string) bool {
	if u.catalog.contains(u.setup[name]) {
		return true
	}
	for _, value := range u.operations {
		if value.source.Export == name && u.catalog.contains(value) {
			return true
		}
	}
	return false
}

func (u *unitExports) operation(operationID string) *publication {
	value := u.operations[operationID]
	if !u.catalog.contains(value) {
		return nil
	}
	return value
}

func (u *unitExports) withdraw() {
	for _, value := range u.setup {
		u.catalog.withdraw(value)
	}
	for _, value := range u.operations {
		u.catalog.withdraw(value)
	}
}

func (p *publication) snapshot() snapshotMember {
	return snapshotMember{id: p.id, source: p.source, metadata: p.metadata, instanceID: p.instance.id, binding: p.instance.snapshot.Clone(), durable: p.instance.exports.reconstructible(p)}
}

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

// The entire setup must be admitted before any member becomes visible.
func (h *hostState) prepareSetupExports(ctx context.Context, instance *unitInstance, exports []ExportValue) ([]*publication, error) {
	actual := make(map[string]bool, len(exports))
	result := make([]*publication, 0, len(exports))
	for _, export := range exports {
		if err := validateName("export name", export.Name); err != nil {
			return nil, err
		}
		if actual[export.Name] {
			return nil, fmt.Errorf("%w: duplicate actual export", frame.ErrInvalid)
		}
		actual[export.Name] = true
		declared, found := instance.unit.export(export.Name)
		if found && declared.Contract != export.Contract || !found && !instance.unit.declaresPoint(export.Contract) {
			return nil, fmt.Errorf("%w: actual export was not declared", frame.ErrContractMismatch)
		}
		value, err := h.prepareExport(ctx, instance, export)
		if err != nil {
			return nil, err
		}
		result = append(result, &publication{preparedExport: value, id: rand.Text()})
	}
	for _, declared := range instance.unit.Exports {
		if !actual[declared.Name] {
			return nil, fmt.Errorf("%w: declared export %s missing", frame.ErrContractMismatch, declared.Name)
		}
	}
	return result, nil
}

// prepareExport admits one actual value and a detached metadata snapshot
// without holding Host.mu, because metadata decoding may be slow.
func (h *hostState) prepareExport(ctx context.Context, instance *unitInstance, export ExportValue) (*preparedExport, error) {
	h.mu.Lock()
	definition := h.contracts.lookup(export.Contract)
	types := h.contracts.goTypes[export.Contract]
	driver := h.drivers[instance.unit.Profile]
	h.mu.Unlock()
	if definition == nil {
		return nil, frame.ErrContractMismatch
	}
	if validator, ok := driver.Driver.(ExportValidator); ok {
		if err := instance.validateCurrent(ctx); err != nil {
			return nil, err
		}
		if err := validator.ValidateExport(instance.id, export); err != nil {
			return nil, err
		}
	} else if export.Contract.Kind != EventKind {
		if framevalue.IsNil(export.Value) {
			return nil, fmt.Errorf("%w: nil actual export", frame.ErrContractMismatch)
		}
		if types.value == nil {
			return nil, fmt.Errorf("%w: export has no local type binding", frame.ErrContractMismatch)
		}
		if !reflect.TypeOf(export.Value).AssignableTo(types.value) {
			return nil, frame.ErrContractMismatch
		}
	}
	if err := instance.validateCurrent(ctx); err != nil {
		return nil, err
	}
	metadata, err := prepareMetadata(definition, types.metadata, export.Metadata)
	if err != nil {
		return nil, err
	}
	return &preparedExport{source: instance.exportRef(export.Name, export.Contract), metadata: metadata, value: export.Value, instance: instance}, nil
}

// PublicationRequest fixes one native implementation and metadata snapshot. A
// replay must retain this request because arbitrary Go values have no structural
// identity that can safely distinguish implementations, closures or pointers.
type PublicationRequest struct{ *preparedExport }

// Valid reports whether the request fixes a prepared export; the zero request does not.
func (r PublicationRequest) Valid() bool { return r.preparedExport != nil }

func (h *hostState) PreparePublication(ctx context.Context, instanceID string, value ExportValue) (PublicationRequest, error) {
	if err := frame.ContextError(ctx); err != nil {
		return PublicationRequest{}, err
	}
	h.mu.Lock()
	instance := h.allInstances[instanceID]
	err := ErrUnavailable
	var work frame.Work
	if instance != nil {
		err = instance.validateServingLocked()
		if err == nil {
			work, err = instance.lifetime.Enter(ctx)
		}
	}
	h.mu.Unlock()
	if err != nil {
		return PublicationRequest{}, err
	}
	defer work.Finish()
	if !instance.unit.declaresPoint(value.Contract) {
		return PublicationRequest{}, frame.ErrContractMismatch
	}
	if err := validateName("export name", value.Name); err != nil {
		return PublicationRequest{}, err
	}
	prepared, err := h.prepareExport(work.Context(), instance, value)
	if err != nil {
		return PublicationRequest{}, err
	}
	if err := instance.validateCurrent(work.Context()); err != nil {
		return PublicationRequest{}, err
	}
	return PublicationRequest{preparedExport: prepared}, nil
}

// A returned error proves this call did not publish. An earlier successful
// operation remains authoritative and can only be replayed with its request.
func (h *hostState) Publish(ctx context.Context, operationID string, request PublicationRequest) (Publication, error) {
	if request.preparedExport == nil || request.instance.host != h {
		return Publication{}, frame.ErrInvalid
	}
	if err := validateName("publication operation", operationID); err != nil {
		return Publication{}, err
	}
	candidate := &publication{preparedExport: request.preparedExport, id: rand.Text()}
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
		value, err := instance.exports.publish(operationID, candidate)
		if err != nil {
			return err
		}
		result = value.view()
		return nil
	})
	return result, err
}

func (h *hostState) Publication(instanceID, operationID string) (Publication, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	instance := h.allInstances[instanceID]
	if instance == nil {
		return Publication{}, ErrUnavailable
	}
	value := instance.exports.operation(operationID)
	if value == nil {
		return Publication{}, ErrUnavailable
	}
	return value.view(), nil
}

func (h *hostState) Withdraw(ctx context.Context, id string) error {
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
