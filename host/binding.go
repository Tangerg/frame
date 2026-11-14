package host

import (
	"cmp"
	json "encoding/json/v2"
	"fmt"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type Dependency struct {
	InstallationID InstallationID    `json:"installationId"`
	Release        framevalue.Digest `json:"release"`
}

func (d Dependency) Validate() error {
	if err := d.InstallationID.Validate(); err != nil {
		return err
	}
	if err := d.Release.Validate(); err != nil {
		return fmt.Errorf("%w: dependency release: %w", frame.ErrInvalid, err)
	}
	return nil
}

func CompactDependencies(values []Dependency) []Dependency {
	result := slices.Clone(values)
	slices.SortFunc(result, func(a, b Dependency) int {
		return cmp.Or(cmp.Compare(a.InstallationID, b.InstallationID), a.Release.Compare(b.Release))
	})
	return slices.Compact(result)
}

type UnitRef struct {
	InstallationID InstallationID    `json:"installationId"`
	Release        framevalue.Digest `json:"release"`
	UnitID         UnitID            `json:"unitId"`
}

func (r UnitRef) Validate() error {
	if err := (Dependency{InstallationID: r.InstallationID, Release: r.Release}).Validate(); err != nil {
		return err
	}
	return r.UnitID.Validate()
}

func (r UnitRef) String() string { return r.InstallationID.String() + "/" + r.UnitID.String() }

func (r UnitRef) Dependency() Dependency {
	return Dependency{InstallationID: r.InstallationID, Release: r.Release}
}

type ExportRef struct {
	InstallationID InstallationID    `json:"installationId"`
	Release        framevalue.Digest `json:"release"`
	UnitID         UnitID            `json:"unitId"`
	Export         string            `json:"export"`
	Contract       ContractRef       `json:"contract"`
}

func (r ExportRef) Unit() UnitRef {
	return UnitRef{InstallationID: r.InstallationID, Release: r.Release, UnitID: r.UnitID}
}
func (r ExportRef) Validate() error {
	if err := r.Unit().Validate(); err != nil {
		return err
	}
	if err := validateName("export name", r.Export); err != nil {
		return err
	}
	return r.Contract.Validate()
}

// ImportDeclaration is one slot an author requires. It names no provider:
// selection or the unit's fixed choices decide the exact exports.
type ImportDeclaration struct {
	Slot     string      `json:"slot"`
	Contract ContractRef `json:"contract"`
	Optional bool        `json:"optional"`
}

func (d ImportDeclaration) Validate() error {
	if err := validateName("import slot", d.Slot); err != nil {
		return err
	}
	return d.Contract.Validate()
}

// validateProviders checks one exact provider set against the slot's contract
// and cardinality. An empty set is absence, admitted only for an optional slot.
func (d ImportDeclaration) validateProviders(providers []ExportRef) error {
	if !d.Optional && len(providers) == 0 || d.Contract.Kind == ServiceKind && len(providers) > 1 {
		return fmt.Errorf("%w: import cardinality", frame.ErrInvalid)
	}
	seen := make(map[ExportRef]bool, len(providers))
	for _, provider := range providers {
		if err := provider.Validate(); err != nil {
			return err
		}
		if provider.Contract != d.Contract {
			return frame.ErrContractMismatch
		}
		if seen[provider] {
			return fmt.Errorf("%w: duplicate import provider", frame.ErrInvalid)
		}
		seen[provider] = true
	}
	return nil
}

// SlotProviders fixes the exact providers of one declared slot. Its presence is
// the decision, so an empty set unambiguously chooses absence.
type SlotProviders struct {
	Slot      string      `json:"slot"`
	Providers []ExportRef `json:"providers"`
}

func (p SlotProviders) Clone() SlotProviders {
	p.Providers = slices.Clone(p.Providers)
	return p
}

func (p SlotProviders) equal(other SlotProviders) bool {
	return p.Slot == other.Slot && slices.Equal(p.Providers, other.Providers)
}

type ExportDeclaration struct {
	Name     string      `json:"name"`
	Contract ContractRef `json:"contract"`
}

// UnitDeclaration is immutable release metadata. Fixed slots are wired by the
// declaration itself; every other import is resolved when a binding is planned.
type UnitDeclaration struct {
	ID      UnitID              `json:"id"`
	Profile Profile             `json:"profile"`
	Imports []ImportDeclaration `json:"imports"`
	Exports []ExportDeclaration `json:"exports"`
	Fixed   []SlotProviders     `json:"fixed"`
}

func (d UnitDeclaration) Clone() UnitDeclaration {
	d.Imports = slices.Clone(d.Imports)
	d.Exports = slices.Clone(d.Exports)
	d.Fixed = slices.Clone(d.Fixed)
	for i := range d.Fixed {
		d.Fixed[i] = d.Fixed[i].Clone()
	}
	return d
}

func (d UnitDeclaration) importSlot(slot string) (ImportDeclaration, bool) {
	for _, declared := range d.Imports {
		if declared.Slot == slot {
			return declared, true
		}
	}
	return ImportDeclaration{}, false
}

func (d UnitDeclaration) fixedProviders(slot string) ([]ExportRef, bool) {
	for _, fixed := range d.Fixed {
		if fixed.Slot == slot {
			return fixed.Providers, true
		}
	}
	return nil, false
}

// declaresPoint permits dynamic membership in a Point the unit declared. A
// service export cannot be added or replaced after setup.
func (d UnitDeclaration) declaresPoint(contract ContractRef) bool {
	return contract.Kind == PointKind && slices.ContainsFunc(d.Exports, func(export ExportDeclaration) bool { return export.Contract == contract })
}

func (d UnitDeclaration) export(name string) (ExportDeclaration, bool) {
	for _, export := range d.Exports {
		if export.Name == name {
			return export, true
		}
	}
	return ExportDeclaration{}, false
}

func (d UnitDeclaration) equal(other UnitDeclaration) bool {
	return d.ID == other.ID && d.Profile == other.Profile && slices.Equal(d.Imports, other.Imports) &&
		slices.Equal(d.Exports, other.Exports) && slices.EqualFunc(d.Fixed, other.Fixed, SlotProviders.equal)
}

func (d UnitDeclaration) Validate() error {
	if err := d.ID.Validate(); err != nil {
		return err
	}
	if err := d.Profile.Validate(); err != nil {
		return err
	}
	exports := make(map[string]bool, len(d.Exports))
	for _, value := range d.Exports {
		if err := validateName("export name", value.Name); err != nil {
			return err
		}
		if exports[value.Name] {
			return fmt.Errorf("%w: duplicate export %s", frame.ErrInvalid, value.Name)
		}
		exports[value.Name] = true
		if err := value.Contract.Validate(); err != nil {
			return err
		}
	}
	slots := make(map[string]bool, len(d.Imports))
	for _, value := range d.Imports {
		if err := value.Validate(); err != nil {
			return err
		}
		if slots[value.Slot] {
			return fmt.Errorf("%w: duplicate import slot %s", frame.ErrInvalid, value.Slot)
		}
		slots[value.Slot] = true
	}
	return d.validateFixed()
}

func (d UnitDeclaration) validateFixed() error {
	fixed := make(map[string]bool, len(d.Fixed))
	for _, choice := range d.Fixed {
		declared, found := d.importSlot(choice.Slot)
		if !found {
			return fmt.Errorf("%w: fixed providers for undeclared slot %s", frame.ErrInvalid, choice.Slot)
		}
		if fixed[choice.Slot] {
			return fmt.Errorf("%w: duplicate fixed slot %s", frame.ErrInvalid, choice.Slot)
		}
		fixed[choice.Slot] = true
		if err := declared.validateProviders(choice.Providers); err != nil {
			return err
		}
	}
	return nil
}

// ImportBinding is a declared slot together with the exact providers an
// admitted binding resolved for it; an empty set is resolved absence.
type ImportBinding struct {
	ImportDeclaration
	Providers []ExportRef `json:"providers"`
}

func (b ImportBinding) Clone() ImportBinding {
	b.Providers = slices.Clone(b.Providers)
	return b
}

func (b ImportBinding) equal(other ImportBinding) bool {
	return b.ImportDeclaration == other.ImportDeclaration && slices.Equal(b.Providers, other.Providers)
}

func (b ImportBinding) Validate() error {
	if err := b.ImportDeclaration.Validate(); err != nil {
		return err
	}
	return b.validateProviders(b.Providers)
}

type UnitBinding struct {
	Source        UnitRef           `json:"source"`
	Configuration framevalue.Digest `json:"configuration"`
	Imports       []ImportBinding   `json:"imports"`
}

func (b UnitBinding) Clone() UnitBinding {
	b.Imports = slices.Clone(b.Imports)
	for i := range b.Imports {
		b.Imports[i] = b.Imports[i].Clone()
	}
	return b
}

func (b UnitBinding) canonical() UnitBinding {
	b = b.Clone()
	slices.SortFunc(b.Imports, func(a, b ImportBinding) int { return cmp.Compare(a.Slot, b.Slot) })
	return b
}

func (b UnitBinding) equal(other UnitBinding) bool {
	left, right := b.canonical(), other.canonical()
	return left.Source == right.Source && left.Configuration == right.Configuration && slices.EqualFunc(left.Imports, right.Imports, ImportBinding.equal)
}

// BindingSnapshot is immutable routing evidence when retained by its domain
// owner. It contains no running pointer, publication grant or business outcome.
// Selection identifies the consumer ordering rules a capture applied. Setup
// bindings have none: provider rules belong to the consumer, not setup identity.
type BindingSnapshot struct {
	Roots     []ExportRef       `json:"roots"`
	UnitRoots []UnitRef         `json:"unitRoots"`
	Units     []UnitBinding     `json:"units"`
	Selection framevalue.Digest `json:"selection,omitzero"`
}

// ParseBinding decodes retained routing evidence without selecting providers.
// The original owner must still validate it under admission before a new use.
func ParseBinding(data []byte) (BindingSnapshot, error) {
	var binding BindingSnapshot
	if err := json.Unmarshal(data, &binding, json.RejectUnknownMembers(true)); err != nil {
		return BindingSnapshot{}, err
	}
	if err := binding.Validate(); err != nil {
		return BindingSnapshot{}, err
	}
	return binding, nil
}

func (b BindingSnapshot) Clone() BindingSnapshot {
	b.Roots = slices.Clone(b.Roots)
	b.UnitRoots = slices.Clone(b.UnitRoots)
	b.Units = slices.Clone(b.Units)
	for i := range b.Units {
		b.Units[i] = b.Units[i].Clone()
	}
	return b
}

func compareUnit(a, b UnitRef) int {
	return cmp.Or(cmp.Compare(a.InstallationID, b.InstallationID), a.Release.Compare(b.Release), cmp.Compare(a.UnitID, b.UnitID))
}

func compareExport(a, b ExportRef) int {
	return cmp.Or(compareUnit(a.Unit(), b.Unit()), cmp.Compare(a.Export, b.Export))
}

func (b BindingSnapshot) sameIdentity(other BindingSnapshot) (bool, error) {
	left, err := b.Identity()
	if err != nil {
		return false, err
	}
	right, err := other.Identity()
	return left == right, err
}

func (b BindingSnapshot) Validate() error {
	units := make(map[UnitRef]UnitBinding, len(b.Units))
	for _, unit := range b.Units {
		if err := unit.validate(); err != nil {
			return err
		}
		if _, found := units[unit.Source]; found {
			return fmt.Errorf("%w: duplicate bound unit", frame.ErrInvalid)
		}
		units[unit.Source] = unit
	}
	if err := b.validateRoots(units); err != nil {
		return err
	}
	return b.validateReachability(units)
}

func (u UnitBinding) validate() error {
	if err := u.Source.Validate(); err != nil {
		return err
	}
	if err := u.Configuration.Validate(); err != nil {
		return err
	}
	slots := make(map[string]bool)
	for _, binding := range u.Imports {
		if slots[binding.Slot] {
			return fmt.Errorf("%w: duplicate bound import", frame.ErrInvalid)
		}
		slots[binding.Slot] = true
		if err := binding.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (b BindingSnapshot) validateRoots(units map[UnitRef]UnitBinding) error {
	for _, root := range b.Roots {
		if err := root.Validate(); err != nil {
			return err
		}
		if _, exists := units[root.Unit()]; !exists {
			return fmt.Errorf("%w: root unit missing from binding", frame.ErrInvalid)
		}
	}
	unitRoots := make(map[UnitRef]bool, len(b.UnitRoots))
	for _, root := range b.UnitRoots {
		if err := root.Validate(); err != nil {
			return err
		}
		if _, exists := units[root]; !exists {
			return fmt.Errorf("%w: root unit missing from binding", frame.ErrInvalid)
		}
		if unitRoots[root] {
			return fmt.Errorf("%w: duplicate root unit", frame.ErrInvalid)
		}
		unitRoots[root] = true
	}
	return nil
}

// validateReachability requires every bound unit to be reachable from a root
// through acyclic provider edges.
func (b BindingSnapshot) validateReachability(units map[UnitRef]UnitBinding) error {
	visiting, visited := make(map[UnitRef]bool), make(map[UnitRef]bool)
	var visit func(UnitRef) error
	visit = func(ref UnitRef) error {
		if visiting[ref] {
			return fmt.Errorf("%w: %s/%s", ErrDependencyCycle, ref.InstallationID, ref.UnitID)
		}
		if visited[ref] {
			return nil
		}
		unit, found := units[ref]
		if !found {
			return fmt.Errorf("%w: bound provider unit missing", frame.ErrInvalid)
		}
		visiting[ref] = true
		for _, imp := range unit.Imports {
			for _, provider := range imp.Providers {
				if provider.Unit() == ref {
					continue
				}
				if err := visit(provider.Unit()); err != nil {
					return err
				}
			}
		}
		delete(visiting, ref)
		visited[ref] = true
		return nil
	}
	for _, root := range b.Roots {
		if err := visit(root.Unit()); err != nil {
			return err
		}
	}
	for _, root := range b.UnitRoots {
		if err := visit(root); err != nil {
			return err
		}
	}
	if len(visited) != len(units) {
		return fmt.Errorf("%w: binding includes unreachable units", frame.ErrInvalid)
	}
	return nil
}

func (b BindingSnapshot) requiresInstallation(id InstallationID) bool {
	return slices.ContainsFunc(b.Units, func(unit UnitBinding) bool {
		return unit.Source.InstallationID == id
	})
}

func (b BindingSnapshot) Dependencies() []Dependency {
	values := make([]Dependency, 0, len(b.Units))
	for _, unit := range b.Units {
		values = append(values, unit.Source.Dependency())
	}
	return CompactDependencies(values)
}

// CanonicalJSON is the identity spelling: units and slots are sorted, while
// root and provider order remain part of the accepted binding.
func (b BindingSnapshot) CanonicalJSON() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	value := b.Clone()
	slices.SortFunc(value.Units, func(a, b UnitBinding) int { return compareUnit(a.Source, b.Source) })
	for i := range value.Units {
		value.Units[i] = value.Units[i].canonical()
	}
	return json.Marshal(value, json.Deterministic(true))
}

func (b BindingSnapshot) Identity() (framevalue.Digest, error) {
	data, err := b.CanonicalJSON()
	if err != nil {
		return framevalue.Digest{}, err
	}
	return framevalue.Hash(data), nil
}
