package frame

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	framevalue "github.com/Tangerg/frame/value"
)

type authorityIdentity struct{ marker byte }

type publication struct {
	id       string
	source   ExportRef
	metadata any
	value    any
	instance *Instance
	durable  bool
	visible  bool
	revision uint64
}

type pointStore struct {
	revision uint64
	members  map[string]*publication
}

type snapshotMember struct {
	id         string
	source     ExportRef
	metadata   any
	instanceID string
	binding    BindingSnapshot
	durable    bool
}

type pointRead struct {
	ref      ContractRef
	revision uint64
}

type Snapshot struct {
	owner   *authorityIdentity
	reads   []pointRead
	members map[ContractRef][]snapshotMember
	realm   RealmID
}

type Candidate[M, V any] struct {
	snapshot *Snapshot
	member   snapshotMember
}

func (c Candidate[M, V]) Source() ExportRef { return c.member.source }
func (c Candidate[M, V]) ID() string        { return c.member.id }
func (c Candidate[M, V]) Metadata() M {
	var zero M
	if c.member.metadata == nil {
		return zero
	}
	value, err := snapshotMetadata(c.member.metadata)
	if err != nil {
		panic("frame: admitted metadata became invalid")
	}
	return value.(M)
}

type SelectionRef struct {
	id       string
	snapshot *Snapshot
	contract ContractRef
	rule     string
	members  []snapshotMember
}

type Selection[M, V any] struct{ value SelectionRef }

func (s Selection[M, V]) Ref() SelectionRef { return s.value }

type BindingPlan struct {
	owner       *authorityIdentity
	reads       []pointRead
	members     []snapshotMember
	selections  []SelectionRef
	binding     BindingSnapshot
	realm       RealmID
	durable     bool
	unitMembers []unitMember
	preparation *bindingPreparation
}

type unitMember struct {
	source     UnitRef
	instanceID string
}

// Snapshot is an immutable data projection, not admission or a commit receipt.
// A new business record validates its original plan with ValidatePlanUnder in
// that record's shared admission before retaining this exact graph.
func (p *BindingPlan) Snapshot() (BindingSnapshot, error) {
	if p == nil {
		return BindingSnapshot{}, ErrInvalid
	}
	if !p.durable {
		return BindingSnapshot{}, fmt.Errorf("%w: dynamic publication has no stable reconstruction contract", ErrInvalid)
	}
	return p.binding.Clone(), nil
}

func declaresPoint(unit UnitDeclaration, contract ContractRef) bool {
	if contract.Kind != PointKind {
		return false
	}
	for _, declared := range unit.Exports {
		if declared.Contract == contract {
			return true
		}
	}
	return false
}

func (h *Host) prepareExports(instance *Instance, exports []ExportValue) ([]*publication, error) {
	h.mu.Lock()
	declarations := make(map[ContractRef]*declaration, len(h.contracts))
	for ref, definition := range h.contracts {
		declarations[ref] = definition
	}
	h.mu.Unlock()
	seen := make(map[string]bool)
	result := make([]*publication, 0, len(exports))
	for _, export := range exports {
		if err := validateName("export name", export.Name); err != nil {
			return nil, err
		}
		if seen[export.Name] {
			return nil, fmt.Errorf("%w: duplicate actual export", ErrInvalid)
		}
		seen[export.Name] = true
		definition := declarations[export.Contract]
		if definition == nil {
			return nil, ErrContractMismatch
		}
		declared := false
		reservedName := false
		for _, expected := range instance.unit.Exports {
			if expected.Name == export.Name {
				reservedName = true
				declared = expected.Contract == export.Contract
			}
		}
		if !declared {
			if reservedName || !declaresPoint(instance.unit, export.Contract) {
				return nil, fmt.Errorf("%w: actual export was not declared", ErrContractMismatch)
			}
			export.Durable = false
		}
		if export.Contract.Kind != EventKind && nilValue(export.Value) {
			return nil, fmt.Errorf("%w: nil actual export", ErrContractMismatch)
		}
		if export.Contract.Kind != EventKind && definition.valueType != nil {
			value := reflect.TypeOf(export.Value)
			if value == nil || !value.AssignableTo(definition.valueType) {
				return nil, ErrContractMismatch
			}
		}
		metadata, err := prepareMetadata(definition, export.Metadata)
		if err != nil {
			return nil, err
		}
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		result = append(result, &publication{id: id, source: ExportRef{InstallationID: instance.binding.Source.InstallationID, Release: instance.binding.Source.Release, UnitID: instance.binding.Source.UnitID, Export: export.Name, Contract: export.Contract}, metadata: metadata, value: export.Value, instance: instance, durable: export.Durable})
	}
	for _, expected := range instance.unit.Exports {
		if !seen[expected.Name] {
			return nil, fmt.Errorf("%w: declared export %s missing", ErrContractMismatch, expected.Name)
		}
	}
	return result, nil
}

func (h *Host) publishLocked(value *publication) {
	store := h.catalog[value.source.Contract]
	if store == nil {
		store = &pointStore{members: make(map[string]*publication)}
		h.catalog[value.source.Contract] = store
	}
	store.members[value.id] = value
	value.visible = true
	store.revision++
	value.revision = store.revision
	h.notifyLocked(value.source.Contract, store.revision)
}

func (h *Host) publicationCountLocked() int {
	count := 0
	for _, point := range h.catalog {
		count += len(point.members)
	}
	return count
}

func (h *Host) withdrawLocked(value *publication) {
	if !value.visible {
		return
	}
	value.visible = false
	store := h.catalog[value.source.Contract]
	delete(store.members, value.id)
	store.revision++
	h.notifyLocked(value.source.Contract, store.revision)
}

func (h *Host) withdrawInstanceLocked(instance *Instance) {
	for _, value := range instance.setupExports {
		h.withdrawLocked(value)
	}
	for _, operation := range instance.publicationOperations {
		h.withdrawLocked(operation.publication)
	}
}

func (h *Host) snapshotLocked(realm RealmID, points []ContractRef) (*Snapshot, error) {
	result := &Snapshot{owner: h.identity, members: make(map[ContractRef][]snapshotMember), realm: realm}
	for _, point := range points {
		if h.contracts[point] == nil {
			return nil, ErrContractMismatch
		}
		if _, duplicate := result.members[point]; duplicate {
			return nil, fmt.Errorf("%w: duplicate snapshot contract", ErrInvalid)
		}
		members := []snapshotMember{}
		var revision uint64
		if store := h.catalog[point]; store != nil {
			revision = store.revision
			for _, publication := range store.members {
				if publication.instance.realm != realm && point.Mode == NativeMode {
					continue
				}
				members = append(members, snapshotMember{id: publication.id, source: publication.source, metadata: publication.metadata, instanceID: publication.instance.id, binding: publication.instance.snapshot.Clone(), durable: publication.durable})
			}
		}
		slices.SortFunc(members, func(a, b snapshotMember) int {
			if value := compareUnit(a.source.Unit(), b.source.Unit()); value != 0 {
				return value
			}
			if a.source.Export < b.source.Export {
				return -1
			}
			if a.source.Export > b.source.Export {
				return 1
			}
			if a.id < b.id {
				return -1
			}
			if a.id > b.id {
				return 1
			}
			return 0
		})
		result.reads = append(result.reads, pointRead{ref: point, revision: revision})
		result.members[point] = members
	}
	return result, nil
}

func (h *Host) Snapshot(ctx context.Context, points ...ContractRef) (*Snapshot, error) {
	return h.SnapshotIn(ctx, "", points...)
}
func (h *Host) SnapshotIn(ctx context.Context, realm RealmID, points ...ContractRef) (*Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	return h.snapshotLocked(realm, points)
}

func Candidates[M, V any](snapshot *Snapshot, point Point[M, V]) ([]Candidate[M, V], error) {
	if snapshot == nil || point.value == nil {
		return nil, ErrInvalid
	}
	members, found := snapshot.members[point.Ref()]
	if !found {
		return nil, ErrContractMismatch
	}
	result := make([]Candidate[M, V], len(members))
	for index, member := range members {
		if member.metadata != nil {
			if _, ok := member.metadata.(M); !ok {
				return nil, ErrContractMismatch
			}
		}
		result[index] = Candidate[M, V]{snapshot: snapshot, member: member}
	}
	return result, nil
}

func Select[M, V any](snapshot *Snapshot, point Point[M, V], rule string, candidates ...Candidate[M, V]) (Selection[M, V], error) {
	if point.value == nil {
		return Selection[M, V]{}, ErrInvalid
	}
	refs := make([]CandidateRef, len(candidates))
	for i, candidate := range candidates {
		refs[i] = CandidateRef{snapshot: candidate.snapshot, member: candidate.member}
	}
	ref, err := SelectRefs(snapshot, point.Ref(), rule, refs...)
	return Selection[M, V]{value: ref}, err
}

func SelectOne[M, V any](snapshot *Snapshot, point Point[M, V], rule string, choose func([]Candidate[M, V]) (Candidate[M, V], error)) (Selection[M, V], error) {
	if choose == nil {
		return Selection[M, V]{}, ErrInvalid
	}
	candidates, err := Candidates(snapshot, point)
	if err != nil {
		return Selection[M, V]{}, err
	}
	selected, err := choose(candidates)
	if err != nil {
		return Selection[M, V]{}, err
	}
	return Select(snapshot, point, rule, selected)
}

func SelectMany[M, V any](snapshot *Snapshot, point Point[M, V], rule string, choose func([]Candidate[M, V]) ([]Candidate[M, V], error)) (Selection[M, V], error) {
	if choose == nil {
		return Selection[M, V]{}, ErrInvalid
	}
	candidates, err := Candidates(snapshot, point)
	if err != nil {
		return Selection[M, V]{}, err
	}
	selected, err := choose(candidates)
	if err != nil {
		return Selection[M, V]{}, err
	}
	return Select(snapshot, point, rule, selected...)
}

func Capture(snapshot *Snapshot, selections ...SelectionRef) (*BindingPlan, error) {
	if snapshot == nil {
		return nil, ErrInvalid
	}
	plan := &BindingPlan{owner: snapshot.owner, reads: slices.Clone(snapshot.reads), selections: slices.Clone(selections), realm: snapshot.realm, durable: true, binding: newBindingSnapshot(nil, nil, nil)}
	units := make(map[UnitRef]UnitBinding)
	rules := make([]struct {
		Contract ContractRef `json:"contract"`
		Rule     string      `json:"rule"`
	}, 0, len(selections))
	for _, selection := range selections {
		if selection.snapshot != snapshot || selection.id == "" {
			return nil, ErrInvalid
		}
		rules = append(rules, struct {
			Contract ContractRef `json:"contract"`
			Rule     string      `json:"rule"`
		}{selection.contract, selection.rule})
		for _, member := range selection.members {
			plan.members = append(plan.members, member)
			plan.binding.Roots = append(plan.binding.Roots, member.source)
			plan.durable = plan.durable && member.durable
			for _, unit := range member.binding.Units {
				if previous, exists := units[unit.Source]; exists && !sameUnitBinding(previous, unit) {
					return nil, fmt.Errorf("%w: incompatible setup bindings", ErrInvalid)
				}
				units[unit.Source] = unit.Clone()
			}
		}
	}
	for _, unit := range units {
		plan.binding.Units = append(plan.binding.Units, unit)
	}
	if len(rules) > 0 {
		encoded, err := json.Marshal(rules)
		if err != nil {
			return nil, err
		}
		plan.binding.Rule = framevalue.Hash(encoded).String()
	}
	ordered, err := subsetBinding(plan.binding, plan.binding.Roots)
	if err != nil {
		return nil, err
	}
	plan.binding = ordered
	return plan, nil
}

func (h *Host) validatePlanLocked(plan *BindingPlan) error {
	if plan == nil || plan.owner != h.identity {
		return ErrStaleBinding
	}
	if h.closed {
		return ErrClosed
	}
	if plan.preparation != nil {
		if err := h.validateBindingLocked(plan.binding); err != nil {
			return err
		}
		for _, bound := range plan.binding.Units {
			unit, _, err := h.unitLocked(bound.Source)
			if err != nil {
				return err
			}
			realm, err := h.realmLocked(unit.Declaration, plan.realm)
			if err != nil {
				return err
			}
			actual := h.instances[instanceKey{source: bound.Source, realm: realm}]
			if actual != nil && !actual.retiring && !sameUnitBinding(actual.binding, bound) {
				return fmt.Errorf("%w: unit acquired another setup binding", ErrStaleBinding)
			}
			if actual == nil || !actual.readyDone {
				if err := h.validateReadyImportsLocked(bound, plan.realm); err != nil {
					return err
				}
			}
		}
		for _, source := range plan.binding.Roots {
			if err := h.validateReadyExportLocked(source, plan.realm); err != nil {
				return err
			}
		}
		if err := h.validateBindingReadsLocked(plan.preparation.reads); err != nil {
			return err
		}
	}
	for _, read := range plan.reads {
		var revision uint64
		if store := h.catalog[read.ref]; store != nil {
			revision = store.revision
		}
		if revision != read.revision {
			return ErrStaleBinding
		}
	}
	for _, member := range plan.members {
		store := h.catalog[member.source.Contract]
		if store == nil {
			return ErrStaleBinding
		}
		actual := store.members[member.id]
		if actual == nil || actual.source != member.source || actual.instance.id != member.instanceID || actual.instance.retiring {
			return ErrStaleBinding
		}
		if err := h.admission.ValidateScope(actual.instance.scope); err != nil {
			return err
		}
	}
	for _, member := range plan.unitMembers {
		instance := h.allInstances[member.instanceID]
		if instance == nil || instance.binding.Source != member.source || instance.retiring {
			return ErrStaleBinding
		}
		if err := h.admission.ValidateScope(instance.scope); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) validateReadyImportsLocked(bound UnitBinding, realm RealmID) error {
	for _, requirement := range bound.Imports {
		for _, provider := range requirement.Providers {
			if err := h.validateReadyExportLocked(provider, realm); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Host) validateReadyExportLocked(source ExportRef, realm RealmID) error {
	unit, _, err := h.unitLocked(source.Unit())
	if err != nil {
		return err
	}
	realm, err = h.realmLocked(unit.Declaration, realm)
	if err != nil {
		return err
	}
	instance := h.instances[instanceKey{source: source.Unit(), realm: realm}]
	if instance == nil || !instance.readyDone {
		return nil
	}
	if instance.retiring || instance.readyErr != nil {
		return ErrUnavailable
	}
	_, err = instance.setupExportLocked(source)
	return err
}

func (h *Host) ValidatePlanUnder(ctx context.Context, plan *BindingPlan) (BindingSnapshot, error) {
	if err := h.ValidateSelectionUnder(ctx, plan); err != nil {
		return BindingSnapshot{}, err
	}
	return plan.Snapshot()
}

// ValidateSelectionUnder validates a metadata selection without borrowing any
// actual value or promising that dynamic members can be durably reconstructed.
func (h *Host) ValidateSelectionUnder(ctx context.Context, plan *BindingPlan) error {
	if err := h.admission.CheckAdmitted(ctx); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.validatePlanLocked(plan); err != nil {
		return err
	}
	return nil
}

type Publication struct {
	ID         string
	Source     ExportRef
	InstanceID string
	Revision   uint64
}

type CandidateRef struct {
	snapshot *Snapshot
	member   snapshotMember
}

func (c CandidateRef) ID() string         { return c.member.id }
func (c CandidateRef) Source() ExportRef  { return c.member.source }
func (c CandidateRef) InstanceID() string { return c.member.instanceID }
func (c CandidateRef) MetadataJSON() ([]byte, error) {
	if value, ok := c.member.metadata.(framevalue.JSON); ok {
		return value.Bytes(), nil
	}
	value, err := framevalue.EncodeJSON(c.member.metadata)
	if err != nil {
		return nil, err
	}
	return value.Bytes(), nil
}

func (s *Snapshot) Candidates(contract ContractRef) ([]CandidateRef, error) {
	if s == nil {
		return nil, ErrInvalid
	}
	values, found := s.members[contract]
	if !found {
		return nil, ErrContractMismatch
	}
	result := make([]CandidateRef, len(values))
	for i, value := range values {
		result[i] = CandidateRef{snapshot: s, member: value}
	}
	return result, nil
}

func (s *Snapshot) Revision(contract ContractRef) (uint64, error) {
	if s == nil {
		return 0, ErrInvalid
	}
	for _, read := range s.reads {
		if read.ref == contract {
			return read.revision, nil
		}
	}
	return 0, ErrContractMismatch
}

func SelectRefs(snapshot *Snapshot, contract ContractRef, rule string, candidates ...CandidateRef) (SelectionRef, error) {
	if snapshot == nil {
		return SelectionRef{}, ErrInvalid
	}
	if err := validateName("selection rule", rule); err != nil {
		return SelectionRef{}, err
	}
	if _, exists := snapshot.members[contract]; !exists {
		return SelectionRef{}, ErrContractMismatch
	}
	id, err := randomID()
	if err != nil {
		return SelectionRef{}, err
	}
	selection := SelectionRef{id: id, snapshot: snapshot, contract: contract, rule: rule, members: []snapshotMember{}}
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.snapshot != snapshot || candidate.member.source.Contract != contract {
			return SelectionRef{}, ErrContractMismatch
		}
		if seen[candidate.member.id] {
			return SelectionRef{}, ErrInvalid
		}
		seen[candidate.member.id] = true
		selection.members = append(selection.members, candidate.member)
	}
	return selection, nil
}

func (h *Host) DecodeMetadata(contract ContractRef, data []byte) (any, error) {
	value, err := framevalue.ParseJSON(data)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	definition := h.contracts[contract]
	h.mu.Unlock()
	if definition == nil {
		return nil, ErrContractMismatch
	}
	return decodeMetadata(definition, value)
}

func (h *Host) Publish(ctx context.Context, instanceID, operationID string, value ExportValue) (Publication, error) {
	if err := validateName("publication operation", operationID); err != nil {
		return Publication{}, err
	}
	h.mu.Lock()
	instance := h.allInstances[instanceID]
	if instance == nil {
		h.mu.Unlock()
		return Publication{}, ErrUnavailable
	}
	err := instance.validatePublicationLocked()
	h.mu.Unlock()
	if err != nil {
		return Publication{}, err
	}
	// Dynamic membership is permitted only for an explicitly declared Point.
	// It cannot replace a Service or acquire a durable reconstruction identity.
	if !declaresPoint(instance.unit, value.Contract) {
		return Publication{}, ErrContractMismatch
	}
	value.Durable = false
	temporary := Instance{unit: instance.unit.Clone(), binding: instance.binding.Clone()}
	temporary.unit.Exports = []ExportDeclaration{{Name: value.Name, Contract: value.Contract}}
	prepared, err := h.prepareExports(&temporary, []ExportValue{value})
	if err != nil {
		return Publication{}, err
	}
	publication := prepared[0]
	publication.instance = instance
	metadata, err := json.Marshal(publication.metadata)
	if err != nil {
		return Publication{}, err
	}
	fingerprint := framevalue.Hash(append([]byte(value.Name+"\x00"+value.Contract.Digest.String()+"\x00"), metadata...))
	var result Publication
	err = h.admission.Admit(ctx, func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := instance.validatePublicationLocked(); err != nil {
			return err
		}
		if original := instance.publicationOperations[operationID]; original != nil {
			if original.request != fingerprint || !original.publication.visible {
				return ErrStaleBinding
			}
			result = Publication{ID: original.publication.id, Source: original.publication.source, InstanceID: instance.id, Revision: original.publication.revision}
			return nil
		}
		if len(instance.publicationOperations) >= h.maxPublicationOperations {
			return ErrCapacity
		}
		if h.publicationCountLocked() >= h.maxPublications {
			return ErrCapacity
		}
		if instance.hasVisiblePublicationLocked(value.Name) {
			return fmt.Errorf("%w: export is already published", ErrInvalid)
		}
		h.publishLocked(publication)
		instance.publicationOperations[operationID] = &publicationOperation{request: fingerprint, publication: publication}
		result = Publication{ID: publication.id, Source: publication.source, InstanceID: instance.id, Revision: publication.revision}
		return nil
	})
	return result, err
}

func (i *Instance) validatePublicationLocked() error {
	if !i.readyDone || i.readyErr != nil {
		return ErrUnavailable
	}
	return i.validateCurrentLocked()
}

func (i *Instance) hasVisiblePublicationLocked(name string) bool {
	if original := i.setupExports[name]; original != nil && original.visible {
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
	return Publication{ID: operation.publication.id, Source: operation.publication.source, InstanceID: instanceID, Revision: operation.publication.revision}, nil
}

func (h *Host) Withdraw(ctx context.Context, id string) error {
	return h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, store := range h.catalog {
			if value := store.members[id]; value != nil {
				h.withdrawLocked(value)
				return nil
			}
		}
		return ErrUnavailable
	})
}
