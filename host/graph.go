package host

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

func (h *hostState) unitLocked(ref UnitRef) (Unit, *installed, error) {
	installation := h.installations[ref.InstallationID]
	if installation == nil || !installation.record.Enabled || installation.record.Release != ref.Release {
		return Unit{}, nil, ErrUnavailable
	}
	for _, unit := range installation.plugin.units {
		if unit.Declaration.ID == ref.UnitID {
			return unit, installation, nil
		}
	}
	return Unit{}, nil, ErrUnavailable
}

func (h *hostState) exportLocked(ref ExportRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	unit, _, err := h.unitLocked(ref.Unit())
	if err != nil {
		return err
	}
	for _, export := range unit.Declaration.Exports {
		if export.Name == ref.Export && export.Contract == ref.Contract {
			return nil
		}
	}
	return fmt.Errorf("%w: exact export %s", frame.ErrContractMismatch, ref.Export)
}

func (h *hostState) candidatesLocked(contract ContractRef) []ExportRef {
	var result []ExportRef
	for _, installation := range h.installations {
		if !installation.record.Enabled {
			continue
		}
		for _, unit := range installation.record.Units {
			source := installation.record.Unit(unit.ID)
			for _, export := range unit.Exports {
				if export.Contract == contract {
					result = append(result, ExportRef{InstallationID: source.InstallationID, Release: source.Release, UnitID: source.UnitID, Export: export.Name, Contract: contract})
				}
			}
		}
	}
	slices.SortFunc(result, compareExport)
	return result
}

// BindingChoice is a consumer's explicit providers for one unresolved slot.
// An empty, non-nil Providers chooses absence for an optional slot.
type BindingChoice struct {
	Unit      UnitRef
	Slot      string
	Providers []ExportRef
}

type slotKey struct {
	unit UnitRef
	slot string
}

// bindingRead records the candidates a resolution observed for an unresolved
// slot, including their absence; admission rejects the plan if they change.
type bindingRead struct {
	unit       UnitRef
	slot       string
	contract   ContractRef
	candidates []ExportRef
}

func (h *hostState) validateBindingReadsLocked(reads []bindingRead) error {
	for _, read := range reads {
		if !slices.Equal(read.candidates, h.candidatesLocked(read.contract)) {
			return fmt.Errorf("%w: providers changed for %s import %s", ErrStaleBinding, read.unit, read.slot)
		}
	}
	return nil
}

// Plan resolves declarations without starting code or granting a Use. The
// returned plan retains candidate reads, including absence, for admission.
func (h *hostState) Plan(ctx context.Context, roots ...ExportRef) (BindingPlan, error) {
	return h.PlanWith(ctx, "", nil, roots...)
}

// PlanWith accepts a consumer's explicit choices for unresolved slots. The
// choices are an input to this capture, never another mutable routing table.
// An existing instance keeps its original setup binding or rejects the capture.
func (h *hostState) PlanWith(ctx context.Context, realm RealmID, choices []BindingChoice, roots ...ExportRef) (BindingPlan, error) {
	return h.plan(ctx, realm, choices, roots, nil)
}

// PlanUnit captures a unit with no required public export, such as an approved
// background entry. Unit ownership still includes its complete exact imports.
func (h *hostState) PlanUnit(ctx context.Context, realm RealmID, root UnitRef, choices []BindingChoice) (BindingPlan, error) {
	return h.plan(ctx, realm, choices, nil, []UnitRef{root})
}

func (h *hostState) plan(ctx context.Context, realm RealmID, choices []BindingChoice, roots []ExportRef, unitRoots []UnitRef) (BindingPlan, error) {
	if err := frame.ContextError(ctx); err != nil {
		return BindingPlan{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.IsClosing() {
		return BindingPlan{}, frame.ErrClosed
	}
	plan, err := h.resolveLocked(roots, unitRoots, realm, choices)
	return BindingPlan{plan}, err
}

func (h *hostState) resolveLocked(roots []ExportRef, unitRoots []UnitRef, realm RealmID, choices []BindingChoice) (*bindingPlan, error) {
	chosen, err := h.indexChoicesLocked(choices)
	if err != nil {
		return nil, err
	}
	resolution := &bindingResolution{host: h, realm: realm, chosen: chosen, visiting: make(map[UnitRef]bool), visited: make(map[UnitRef]bool)}
	for _, root := range roots {
		if err := h.exportLocked(root); err != nil {
			return nil, err
		}
		if err := resolution.resolve(root.Unit()); err != nil {
			return nil, err
		}
	}
	for _, root := range unitRoots {
		if err := resolution.resolve(root); err != nil {
			return nil, err
		}
	}
	binding := newBindingSnapshot(roots, unitRoots, resolution.units)
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return &bindingPlan{owner: h.identity, binding: binding, realm: realm, kind: declarationPlan, bindingReads: resolution.reads}, nil
}

func (h *hostState) indexChoicesLocked(choices []BindingChoice) (map[slotKey][]ExportRef, error) {
	chosen := make(map[slotKey][]ExportRef, len(choices))
	for _, choice := range choices {
		if choice.Providers == nil {
			return nil, fmt.Errorf("%w: binding choice without providers", frame.ErrInvalid)
		}
		unit, _, err := h.unitLocked(choice.Unit)
		if err != nil {
			return nil, err
		}
		declared, found := unit.Declaration.importSlot(choice.Slot)
		if !found || declared.Providers != nil && !slices.Equal(declared.Providers, choice.Providers) {
			return nil, frame.ErrContractMismatch
		}
		key := slotKey{unit: choice.Unit, slot: choice.Slot}
		if _, duplicate := chosen[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate binding choice", frame.ErrInvalid)
		}
		chosen[key] = slices.Clone(choice.Providers)
	}
	return chosen, nil
}

// bindingResolution walks provider edges from the roots, binding each reached
// unit once and refusing a dependency cycle.
type bindingResolution struct {
	host     *hostState
	realm    RealmID
	chosen   map[slotKey][]ExportRef
	reads    []bindingRead
	visiting map[UnitRef]bool
	visited  map[UnitRef]bool
	path     []string
	units    []UnitBinding
}

func (r *bindingResolution) resolve(ref UnitRef) error {
	if r.visiting[ref] {
		return fmt.Errorf("%w: %s", ErrDependencyCycle, strings.Join(append(slices.Clone(r.path), ref.String()), " -> "))
	}
	if r.visited[ref] {
		return nil
	}
	h := r.host
	unit, installation, err := h.unitLocked(ref)
	if err != nil {
		return err
	}
	realm, err := h.realmLocked(unit.Declaration, r.realm)
	if err != nil {
		return err
	}
	r.visiting[ref] = true
	r.path = append(r.path, ref.String())
	defer func() { r.path = r.path[:len(r.path)-1] }()
	bound := UnitBinding{Source: ref, Configuration: framevalue.Hash(installation.record.Configuration)}
	requirements := unit.Declaration.Imports
	// A live instance of the same configuration keeps its original providers.
	if existing := h.instances[instanceKey{source: ref, realm: realm}]; existing != nil && existing.binding.Configuration == bound.Configuration {
		requirements = existing.binding.Imports
		r.reads = append(r.reads, existing.preparation.reads...)
	}
	for _, requirement := range requirements {
		providers, err := r.providers(ref, requirement)
		if err != nil {
			return err
		}
		if err := r.resolveProviders(ref, requirement, providers); err != nil {
			return err
		}
		requirement = requirement.Clone()
		requirement.Providers = providers
		bound.Imports = append(bound.Imports, requirement)
	}
	delete(r.visiting, ref)
	r.visited[ref] = true
	r.units = append(r.units, bound)
	return nil
}

func (r *bindingResolution) resolveProviders(ref UnitRef, requirement ImportDeclaration, providers []ExportRef) error {
	for _, provider := range providers {
		if provider.Contract != requirement.Contract {
			return frame.ErrContractMismatch
		}
		if err := r.host.exportLocked(provider); err != nil {
			return err
		}
		if provider.Unit() == ref {
			continue
		}
		if err := r.resolve(provider.Unit()); err != nil {
			return fmt.Errorf("unit %s requires %s: %w", ref, requirement.Slot, err)
		}
	}
	return nil
}

// providers chooses the exports that satisfy one import of ref: a recorded
// binding, the caller's choice, or the sole live candidate.
func (r *bindingResolution) providers(ref UnitRef, requirement ImportDeclaration) ([]ExportRef, error) {
	providers := requirement.Providers
	if providers == nil {
		r.reads = append(r.reads, bindingRead{unit: ref, slot: requirement.Slot, contract: requirement.Contract, candidates: r.host.candidatesLocked(requirement.Contract)})
	}
	if chosen, exists := r.chosen[slotKey{unit: ref, slot: requirement.Slot}]; exists {
		if providers != nil && !slices.Equal(providers, chosen) {
			return nil, fmt.Errorf("%w: existing setup binding for %s", ErrStaleBinding, requirement.Slot)
		}
		providers = chosen
	}
	if providers == nil {
		providers = r.host.candidatesLocked(requirement.Contract)
		if requirement.Contract.Kind == ServiceKind && len(providers) > 1 {
			return nil, fmt.Errorf("%w: import %s", ErrAmbiguous, requirement.Slot)
		}
	}
	if len(providers) == 0 && !requirement.Optional {
		return nil, fmt.Errorf("%w: required import %s", ErrUnavailable, requirement.Slot)
	}
	return append([]ExportRef{}, providers...), nil
}

// reachableBinding projects the units reachable from roots, each after all of
// its providers, so instances can be planned in a single pass.
func reachableBinding(source BindingSnapshot, roots []ExportRef, unitRoots []UnitRef) (BindingSnapshot, error) {
	units := make(map[UnitRef]UnitBinding, len(source.Units))
	for _, unit := range source.Units {
		units[unit.Source] = unit
	}
	result := newBindingSnapshot(roots, unitRoots, nil)
	result.Rule = source.Rule
	seen := make(map[UnitRef]bool)
	var visit func(UnitRef) error
	visit = func(ref UnitRef) error {
		if seen[ref] {
			return nil
		}
		unit, found := units[ref]
		if !found {
			return fmt.Errorf("%w: incomplete exact graph", frame.ErrInvalid)
		}
		seen[ref] = true
		for _, requirement := range unit.Imports {
			for _, provider := range requirement.Providers {
				if err := visit(provider.Unit()); err != nil {
					return err
				}
			}
		}
		result.Units = append(result.Units, unit.Clone())
		return nil
	}
	for _, root := range roots {
		if err := visit(root.Unit()); err != nil {
			return BindingSnapshot{}, err
		}
	}
	for _, root := range unitRoots {
		if err := visit(root); err != nil {
			return BindingSnapshot{}, err
		}
	}
	return result, result.Validate()
}

// ValidateBindingUnder checks an original durable identity against current
// installation authority. It never chooses a new provider or updates absence.
func (h *hostState) ValidateBindingUnder(ctx context.Context, binding BindingSnapshot) error {
	if err := h.admission.CheckAdmitted(ctx); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.validateBindingLocked(binding)
}

func (h *hostState) validateBindingLocked(binding BindingSnapshot) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if h.lifetime.IsClosing() {
		return frame.ErrClosed
	}
	for _, unit := range binding.Units {
		declaration, installation, err := h.unitLocked(unit.Source)
		if err != nil {
			return err
		}
		if framevalue.Hash(installation.record.Configuration) != unit.Configuration {
			return ErrStaleBinding
		}
		if err := h.admission.ValidateScope(installation.scope); err != nil {
			return err
		}
		if len(declaration.Declaration.Imports) != len(unit.Imports) {
			return ErrStaleBinding
		}
		for _, bound := range unit.Imports {
			declared, found := declaration.Declaration.importSlot(bound.Slot)
			if !found || declared.Contract != bound.Contract || declared.Optional != bound.Optional ||
				declared.Providers != nil && !slices.Equal(declared.Providers, bound.Providers) {
				return ErrStaleBinding
			}
			for _, provider := range bound.Providers {
				if err := h.exportLocked(provider); err != nil {
					return err
				}
			}
		}
	}
	for _, root := range binding.Roots {
		if err := h.exportLocked(root); err != nil {
			return err
		}
	}
	return nil
}
