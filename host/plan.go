package host

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// BindingPlan is an exact graph awaiting admission. A declared plan still
// needs preparation and carries the candidate reads it depends on; a captured
// plan names the exact catalog members and instances it borrows. Admission
// retains this immutable graph independently of caller-owned handles.
type BindingPlan struct{ plan admissionPlan }

// Valid reports whether the handle names a planned graph; the zero BindingPlan does not.
func (p BindingPlan) Valid() bool { return p.plan != nil }

// admissionPlan is closed over *declaredPlan and *capturedPlan. Each variant
// owns the checks and the acquisition path its own evidence requires.
type admissionPlan interface {
	header() *planHeader
	validateLocked(*hostState) error
	acquire(context.Context, *hostState, UseConfig) (Use, error)
}

type planHeader struct {
	owner   *frame.LifetimeDomain
	realm   RealmID
	binding BindingSnapshot
}

func (p *planHeader) header() *planHeader { return p }

type declaredPlan struct {
	planHeader
	reads []bindingRead
}

type capturedPlan struct {
	planHeader
	catalogReads []pointRead
	members      []snapshotMember
	selections   []SelectionRef
	// Roots name exact executions; their original holds own the provider graph.
	// The binding is its routing projection, not a second instance selector.
	roots map[string]UnitRef
}

func (p *capturedPlan) includeMember(member snapshotMember) {
	p.members = append(p.members, member)
	p.includeRoot(member.instanceID, member.source.Unit())
}

func (p *capturedPlan) includeRoot(id string, source UnitRef) {
	if p.roots == nil {
		p.roots = make(map[string]UnitRef)
	}
	p.roots[id] = source
}

// Snapshot is an immutable data projection, not admission or a commit receipt.
// A new business record validates its original plan with ValidatePlanUnder in
// that record's shared admission before retaining this exact graph.
func (p BindingPlan) Snapshot() (BindingSnapshot, error) {
	switch plan := p.plan.(type) {
	case nil:
		return BindingSnapshot{}, frame.ErrInvalid
	case *capturedPlan:
		for _, member := range plan.members {
			if member.reconstructionError != nil {
				return BindingSnapshot{}, member.reconstructionError
			}
		}
	}
	return p.plan.header().binding.Clone(), nil
}

// selectionRule is the identity of one consumer ordering rule in a capture.
type selectionRule struct {
	Contract ContractRef `json:"contract"`
	Rule     string      `json:"rule"`
}

// Capture freezes selections into one exact graph. The selection rules become
// part of the binding identity; the members' setup bindings must agree.
func Capture(snapshot Snapshot, selections ...SelectionRef) (BindingPlan, error) {
	if !snapshot.Valid() {
		return BindingPlan{}, frame.ErrInvalid
	}
	plan := &capturedPlan{planHeader: planHeader{owner: snapshot.owner, realm: snapshot.realm}, catalogReads: slices.Clone(snapshot.reads), selections: slices.Clone(selections)}
	var graph BindingSnapshot
	units := make(map[UnitRef]UnitBinding)
	rules := make([]selectionRule, 0, len(selections))
	for _, selection := range selections {
		if selection.snapshot != snapshot.catalogSnapshot || selection.id == "" {
			return BindingPlan{}, frame.ErrInvalid
		}
		rules = append(rules, selectionRule{Contract: selection.contract, Rule: selection.rule})
		for _, member := range selection.members {
			plan.includeMember(member)
			graph.Roots = append(graph.Roots, member.source)
			for _, unit := range member.binding.Units {
				if previous, exists := units[unit.Source]; exists && !previous.equal(unit) {
					return BindingPlan{}, fmt.Errorf("%w: incompatible setup bindings", frame.ErrInvalid)
				}
				units[unit.Source] = unit.Clone()
			}
		}
	}
	for _, unit := range units {
		graph.Units = append(graph.Units, unit)
	}
	if len(rules) > 0 {
		encoded, err := json.Marshal(rules, json.Deterministic(true))
		if err != nil {
			return BindingPlan{}, err
		}
		graph.Selection = framevalue.Hash(encoded)
	}
	binding, err := reachableBinding(graph, graph.Roots, nil)
	if err != nil {
		return BindingPlan{}, err
	}
	plan.binding = binding
	return BindingPlan{plan}, nil
}

func (h *hostState) ValidatePlanUnder(ctx context.Context, plan BindingPlan) (BindingSnapshot, error) {
	if err := h.admission.CheckAdmitted(ctx); err != nil {
		return BindingSnapshot{}, err
	}
	h.mu.Lock()
	err := h.validatePlanLocked(plan.plan)
	h.mu.Unlock()
	if err != nil {
		return BindingSnapshot{}, err
	}
	return plan.Snapshot()
}

func (h *hostState) validatePlanLocked(plan admissionPlan) error {
	if plan == nil || plan.header().owner != h.identity {
		return ErrStaleBinding
	}
	if h.lifetime.IsClosing() {
		return frame.ErrClosed
	}
	return plan.validateLocked(h)
}

func (p *declaredPlan) validateLocked(h *hostState) error {
	if err := h.validateBindingLocked(p.binding); err != nil {
		return err
	}
	for _, bound := range p.binding.Units {
		unit, _, err := h.unitLocked(bound.Source)
		if err != nil {
			return err
		}
		realm, err := h.realmLocked(unit.Declaration, p.realm)
		if err != nil {
			return err
		}
		actual := h.instances[instanceKey{source: bound.Source, realm: realm}]
		if actual != nil && !actual.binding.equal(bound) {
			return fmt.Errorf("%w: unit acquired another setup binding", ErrStaleBinding)
		}
		if prepared := actual != nil && actual.preparation.status != preparationPending; prepared {
			continue
		}
		for _, requirement := range bound.Imports {
			for _, provider := range requirement.Providers {
				if err := h.validateProviderLocked(provider, p.realm); err != nil {
					return err
				}
			}
		}
	}
	for _, source := range p.binding.Roots {
		if err := h.validateProviderLocked(source, p.realm); err != nil {
			return err
		}
	}
	return h.validateBindingReadsLocked(p.reads)
}

// A captured plan stays admissible only while every catalog read, member and
// instance it names is unchanged and still serving.
func (p *capturedPlan) validateLocked(h *hostState) error {
	for _, read := range p.catalogReads {
		if h.catalog.revision(read.ref) != read.revision {
			return ErrStaleBinding
		}
	}
	for _, member := range p.members {
		actual := h.catalog.member(member.source.Contract, member.id)
		if actual == nil || actual.source != member.source || actual.instance.id != member.instanceID {
			return ErrStaleBinding
		}
	}
	for id, source := range p.roots {
		instance := h.allInstances[id]
		if instance == nil || instance.binding.Source != source {
			return ErrStaleBinding
		}
		if err := instance.validateServingLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (p *declaredPlan) acquire(ctx context.Context, h *hostState, config UseConfig) (Use, error) {
	return h.acquireActivated(ctx, config, "", h.plannedBinding(p))
}

func (p *capturedPlan) acquire(ctx context.Context, h *hostState, config UseConfig) (Use, error) {
	return h.acquirePrepared(ctx, p, config, "")
}

// validateProviderLocked accepts an export whose instance has not prepared yet;
// a prepared instance must still serve the original setup export.
func (h *hostState) validateProviderLocked(source ExportRef, realm RealmID) error {
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
	switch instance.preparation.status {
	case preparationPending:
		return nil
	case preparationFailed:
		return ErrUnavailable
	}
	_, err = instance.exports.bound(source)
	return err
}
