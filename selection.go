package frame

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	framevalue "github.com/Tangerg/frame/value"
)

// Snapshot is a consistent read of the catalog for selected contracts. Its
// members are projections; Capture rechecks each against the live catalog.
type Snapshot struct {
	owner   *authorityIdentity
	reads   []pointRead
	members map[ContractRef][]snapshotMember
	realm   RealmID
}

type pointRead struct {
	ref      ContractRef
	revision uint64
}

type snapshotMember struct {
	id         string
	source     ExportRef
	metadata   any
	instanceID string
	binding    BindingSnapshot
	durable    bool
}

type Candidate[M, V any] struct {
	snapshot *Snapshot
	member   snapshotMember
}

func (c Candidate[M, V]) Source() ExportRef { return c.member.source }
func (c Candidate[M, V]) ID() string        { return c.member.id }

// Metadata returns a detached copy, so a consumer cannot mutate what other
// candidates of the same publication observe.
func (c Candidate[M, V]) Metadata() (M, error) {
	return readMetadata[M](c.member.metadata)
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
	detached, err := snapshotMetadata(c.member.metadata)
	if err != nil {
		return nil, err
	}
	value, err := framevalue.EncodeJSON(detached)
	if err != nil {
		return nil, err
	}
	return value.Bytes(), nil
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

// BindingPlan is an exact graph awaiting admission. A resolved plan carries
// the candidate reads its preparation depends on; a captured plan carries the
// catalog members its selections chose; a restored plan names ready instances.
type BindingPlan struct {
	owner       *authorityIdentity
	realm       RealmID
	binding     BindingSnapshot
	reads       []pointRead
	members     []snapshotMember
	selections  []SelectionRef
	unitMembers []unitMember
	preparation *bindingPreparation
}

type unitMember struct {
	source     UnitRef
	instanceID string
}

// durable reports whether the plan can be reconstructed from its binding
// alone. A dynamic publication has no setup slot to reconstruct it from.
func (p *BindingPlan) durable() bool {
	return !slices.ContainsFunc(p.members, func(member snapshotMember) bool { return !member.durable })
}

// Snapshot is an immutable data projection, not admission or a commit receipt.
// A new business record validates its original plan with ValidatePlanUnder in
// that record's shared admission before retaining this exact graph.
func (p *BindingPlan) Snapshot() (BindingSnapshot, error) {
	if p == nil {
		return BindingSnapshot{}, ErrInvalid
	}
	if !p.durable() {
		return BindingSnapshot{}, fmt.Errorf("%w: dynamic publication has no stable reconstruction contract", ErrInvalid)
	}
	return p.binding.Clone(), nil
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
	if h.lifetime.isClosing() {
		return nil, ErrClosed
	}
	return h.snapshotLocked(realm, points)
}

// snapshotLocked reads native members only from the requested realm; a
// portable member can be called from any realm.
func (h *Host) snapshotLocked(realm RealmID, points []ContractRef) (*Snapshot, error) {
	result := &Snapshot{owner: h.identity, members: make(map[ContractRef][]snapshotMember, len(points)), realm: realm}
	for _, point := range points {
		if h.contracts.lookup(point) == nil {
			return nil, ErrContractMismatch
		}
		if _, duplicate := result.members[point]; duplicate {
			return nil, fmt.Errorf("%w: duplicate snapshot contract", ErrInvalid)
		}
		members := []snapshotMember{}
		for _, value := range h.catalog.members(point) {
			if point.Mode == NativeMode && value.instance.realm != realm {
				continue
			}
			members = append(members, snapshotMember{id: value.id, source: value.source, metadata: value.metadata, instanceID: value.instance.id, binding: value.instance.snapshot.Clone(), durable: value.durable})
		}
		result.reads = append(result.reads, pointRead{ref: point, revision: h.catalog.revision(point)})
		result.members[point] = members
	}
	return result, nil
}

func (s *Snapshot) Candidates(contract ContractRef) ([]CandidateRef, error) {
	if s == nil {
		return nil, ErrInvalid
	}
	members, found := s.members[contract]
	if !found {
		return nil, ErrContractMismatch
	}
	result := make([]CandidateRef, len(members))
	for i, member := range members {
		result[i] = CandidateRef{snapshot: s, member: member}
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

func Candidates[M, V any](snapshot *Snapshot, point Point[M, V]) ([]Candidate[M, V], error) {
	if snapshot == nil || point.value == nil {
		return nil, ErrInvalid
	}
	members, found := snapshot.members[point.Ref()]
	if !found {
		return nil, ErrContractMismatch
	}
	result := make([]Candidate[M, V], len(members))
	for i, member := range members {
		_, encoded := member.metadata.(framevalue.JSON)
		if member.metadata != nil && !encoded {
			actual := reflect.TypeOf(member.metadata)
			if !actual.AssignableTo(reflect.TypeFor[M]()) {
				return nil, ErrContractMismatch
			}
		}
		result[i] = Candidate[M, V]{snapshot: snapshot, member: member}
	}
	return result, nil
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
	selection := SelectionRef{id: rand.Text(), snapshot: snapshot, contract: contract, rule: rule, members: []snapshotMember{}}
	chosen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate.snapshot != snapshot || candidate.member.source.Contract != contract {
			return SelectionRef{}, ErrContractMismatch
		}
		if chosen[candidate.member.id] {
			return SelectionRef{}, ErrInvalid
		}
		chosen[candidate.member.id] = true
		selection.members = append(selection.members, candidate.member)
	}
	return selection, nil
}

func Select[M, V any](snapshot *Snapshot, point Point[M, V], rule string, candidates ...Candidate[M, V]) (Selection[M, V], error) {
	if point.value == nil {
		return Selection[M, V]{}, ErrInvalid
	}
	refs := make([]CandidateRef, len(candidates))
	for i, candidate := range candidates {
		refs[i] = CandidateRef(candidate)
	}
	ref, err := SelectRefs(snapshot, point.Ref(), rule, refs...)
	return Selection[M, V]{value: ref}, err
}

func SelectOne[M, V any](snapshot *Snapshot, point Point[M, V], rule string, choose func([]Candidate[M, V]) (Candidate[M, V], error)) (Selection[M, V], error) {
	if choose == nil {
		return Selection[M, V]{}, ErrInvalid
	}
	return SelectMany(snapshot, point, rule, func(candidates []Candidate[M, V]) ([]Candidate[M, V], error) {
		chosen, err := choose(candidates)
		return []Candidate[M, V]{chosen}, err
	})
}

func SelectMany[M, V any](snapshot *Snapshot, point Point[M, V], rule string, choose func([]Candidate[M, V]) ([]Candidate[M, V], error)) (Selection[M, V], error) {
	if choose == nil {
		return Selection[M, V]{}, ErrInvalid
	}
	candidates, err := Candidates(snapshot, point)
	if err != nil {
		return Selection[M, V]{}, err
	}
	chosen, err := choose(candidates)
	if err != nil {
		return Selection[M, V]{}, err
	}
	return Select(snapshot, point, rule, chosen...)
}

// selectionRule is the identity of one consumer ordering rule in a capture.
type selectionRule struct {
	Contract ContractRef `json:"contract"`
	Rule     string      `json:"rule"`
}

// Capture freezes selections into one exact graph. The selection rules become
// part of the binding identity; the members' setup bindings must agree.
func Capture(snapshot *Snapshot, selections ...SelectionRef) (*BindingPlan, error) {
	if snapshot == nil {
		return nil, ErrInvalid
	}
	plan := &BindingPlan{owner: snapshot.owner, realm: snapshot.realm, reads: slices.Clone(snapshot.reads), selections: slices.Clone(selections)}
	graph := newBindingSnapshot(nil, nil, nil)
	units := make(map[UnitRef]UnitBinding)
	rules := make([]selectionRule, 0, len(selections))
	for _, selection := range selections {
		if selection.snapshot != snapshot || selection.id == "" {
			return nil, ErrInvalid
		}
		rules = append(rules, selectionRule{Contract: selection.contract, Rule: selection.rule})
		for _, member := range selection.members {
			plan.members = append(plan.members, member)
			graph.Roots = append(graph.Roots, member.source)
			for _, unit := range member.binding.Units {
				if previous, exists := units[unit.Source]; exists && !previous.equal(unit) {
					return nil, fmt.Errorf("%w: incompatible setup bindings", ErrInvalid)
				}
				units[unit.Source] = unit.Clone()
			}
		}
	}
	for _, unit := range units {
		graph.Units = append(graph.Units, unit)
	}
	if len(rules) > 0 {
		encoded, err := json.Marshal(rules)
		if err != nil {
			return nil, err
		}
		graph.Rule = framevalue.Hash(encoded).String()
	}
	binding, err := reachableBinding(graph, graph.Roots, nil)
	if err != nil {
		return nil, err
	}
	plan.binding = binding
	return plan, nil
}

func (h *Host) ValidatePlanUnder(ctx context.Context, plan *BindingPlan) (BindingSnapshot, error) {
	if err := h.admission.CheckAdmitted(ctx); err != nil {
		return BindingSnapshot{}, err
	}
	h.mu.Lock()
	err := h.validatePlanLocked(plan)
	h.mu.Unlock()
	if err != nil {
		return BindingSnapshot{}, err
	}
	return plan.Snapshot()
}

func (h *Host) validatePlanLocked(plan *BindingPlan) error {
	if plan == nil || plan.owner != h.identity {
		return ErrStaleBinding
	}
	if h.lifetime.isClosing() {
		return ErrClosed
	}
	if plan.preparation != nil {
		if err := h.validatePreparationLocked(plan); err != nil {
			return err
		}
	}
	for _, read := range plan.reads {
		if h.catalog.revision(read.ref) != read.revision {
			return ErrStaleBinding
		}
	}
	for _, member := range plan.members {
		actual := h.catalog.member(member.source.Contract, member.id)
		if actual == nil || actual.source != member.source || actual.instance.id != member.instanceID {
			return ErrStaleBinding
		}
		if err := actual.instance.validateServingLocked(); err != nil {
			return err
		}
	}
	for _, member := range plan.unitMembers {
		instance := h.allInstances[member.instanceID]
		if instance == nil || instance.binding.Source != member.source {
			return ErrStaleBinding
		}
		if err := instance.validateServingLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) validatePreparationLocked(plan *BindingPlan) error {
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
		if actual != nil && !actual.binding.equal(bound) {
			return fmt.Errorf("%w: unit acquired another setup binding", ErrStaleBinding)
		}
		if prepared := actual != nil && isClosed(actual.prepared); prepared {
			continue
		}
		for _, requirement := range bound.Imports {
			for _, provider := range requirement.Providers {
				if err := h.validateProviderLocked(provider, plan.realm); err != nil {
					return err
				}
			}
		}
	}
	for _, source := range plan.binding.Roots {
		if err := h.validateProviderLocked(source, plan.realm); err != nil {
			return err
		}
	}
	return h.validateBindingReadsLocked(plan.preparation.reads)
}

// validateProviderLocked accepts an export whose instance has not prepared yet;
// a prepared instance must still serve the original setup export.
func (h *Host) validateProviderLocked(source ExportRef, realm RealmID) error {
	unit, _, err := h.unitLocked(source.Unit())
	if err != nil {
		return err
	}
	realm, err = h.realmLocked(unit.Declaration, realm)
	if err != nil {
		return err
	}
	instance := h.instances[instanceKey{source: source.Unit(), realm: realm}]
	if instance == nil {
		return nil
	}
	done, preparationErr := instance.preparedLocked()
	switch {
	case !done:
		return nil
	case preparationErr != nil:
		return ErrUnavailable
	}
	_, err = instance.setupExportLocked(source)
	return err
}

func isClosed(signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	default:
		return false
	}
}
