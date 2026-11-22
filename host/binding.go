package host

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// ReleaseRef names the exact release installed under one installation. Unit
// and export references compose it, so every reference spells it once.
type ReleaseRef struct {
	InstallationID InstallationID    `json:"installationId"`
	Release        framevalue.Digest `json:"release"`
}

func (r ReleaseRef) Validate() error {
	if err := r.InstallationID.Validate(); err != nil {
		return err
	}
	if err := r.Release.Validate(); err != nil {
		return fmt.Errorf("%w: release: %w", frame.ErrInvalid, err)
	}
	return nil
}

func (r ReleaseRef) compare(other ReleaseRef) int {
	return cmp.Or(cmp.Compare(r.InstallationID, other.InstallationID), r.Release.Compare(other.Release))
}

// CompactReleases orders releases and drops repeats, so equal retention reads
// compare equal however their owners listed them.
func CompactReleases(values []ReleaseRef) []ReleaseRef {
	result := slices.Clone(values)
	slices.SortFunc(result, ReleaseRef.compare)
	return slices.Compact(result)
}

type UnitRef struct {
	ReleaseRef
	UnitID UnitID `json:"unitId"`
}

func (r UnitRef) Validate() error {
	if err := r.ReleaseRef.Validate(); err != nil {
		return err
	}
	return r.UnitID.Validate()
}

func (r UnitRef) String() string { return r.InstallationID.String() + "/" + r.UnitID.String() }

func (r UnitRef) compare(other UnitRef) int {
	return cmp.Or(r.ReleaseRef.compare(other.ReleaseRef), cmp.Compare(r.UnitID, other.UnitID))
}

// Export names one export of this exact unit release.
func (r UnitRef) Export(name string, contract ContractRef) ExportRef {
	return ExportRef{UnitRef: r, Export: name, Contract: contract}
}

type ExportRef struct {
	UnitRef
	Export   string      `json:"export"`
	Contract ContractRef `json:"contract"`
}

func (r ExportRef) Validate() error {
	if err := r.UnitRef.Validate(); err != nil {
		return err
	}
	if err := validateName("export name", r.Export); err != nil {
		return err
	}
	return r.Contract.Validate()
}

func (r ExportRef) String() string { return r.UnitRef.String() + "/" + r.Export }

func (r ExportRef) compare(other ExportRef) int {
	return cmp.Or(r.UnitRef.compare(other.UnitRef), cmp.Compare(r.Export, other.Export))
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

// RequirePortable refuses a native contract in any slot or export. A unit that
// runs outside the Host process cannot share a native Go value with it.
func (d UnitDeclaration) RequirePortable() error {
	for _, required := range d.Imports {
		if required.Contract.Mode != PortableMode {
			return fmt.Errorf("%w: import %q is native", frame.ErrContractMismatch, required.Slot)
		}
	}
	for _, export := range d.Exports {
		if export.Contract.Mode != PortableMode {
			return fmt.Errorf("%w: export %q is native", frame.ErrContractMismatch, export.Name)
		}
	}
	return nil
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
	if err := framevalue.DecodeValue(data, &binding, "binding"); err != nil {
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

func (b BindingSnapshot) sameIdentity(other BindingSnapshot) (bool, error) {
	left, err := b.Canonical()
	if err != nil {
		return false, err
	}
	right, err := other.Canonical()
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

func (b UnitBinding) validate() error {
	if err := b.Source.Validate(); err != nil {
		return err
	}
	if err := b.Configuration.Validate(); err != nil {
		return err
	}
	slots := make(map[string]bool)
	for _, binding := range b.Imports {
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
		if _, exists := units[root.UnitRef]; !exists {
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
	walk := providerWalk{units: units, visiting: make(map[UnitRef]bool), visited: make(map[UnitRef]bool)}
	for _, root := range b.Roots {
		if err := walk.visit(root.UnitRef); err != nil {
			return err
		}
	}
	for _, root := range b.UnitRoots {
		if err := walk.visit(root); err != nil {
			return err
		}
	}
	if len(walk.visited) != len(units) {
		return fmt.Errorf("%w: binding includes unreachable units", frame.ErrInvalid)
	}
	return nil
}

// providerWalk is a depth-first walk; visiting holds the current path, so
// reentering it is a provider cycle rather than a shared provider.
type providerWalk struct {
	units             map[UnitRef]UnitBinding
	visiting, visited map[UnitRef]bool
}

func (w providerWalk) visit(ref UnitRef) error {
	if w.visiting[ref] {
		return fmt.Errorf("%w: %s", ErrDependencyCycle, ref)
	}
	if w.visited[ref] {
		return nil
	}
	unit, found := w.units[ref]
	if !found {
		return fmt.Errorf("%w: bound provider unit missing", frame.ErrInvalid)
	}
	w.visiting[ref] = true
	for _, imp := range unit.Imports {
		for _, provider := range imp.Providers {
			if provider.UnitRef == ref {
				continue
			}
			if err := w.visit(provider.UnitRef); err != nil {
				return err
			}
		}
	}
	delete(w.visiting, ref)
	w.visited[ref] = true
	return nil
}

func (b BindingSnapshot) requiresInstallation(id InstallationID) bool {
	return slices.ContainsFunc(b.Units, func(unit UnitBinding) bool {
		return unit.Source.InstallationID == id
	})
}

func (b BindingSnapshot) Dependencies() []ReleaseRef {
	values := make([]ReleaseRef, 0, len(b.Units))
	for _, unit := range b.Units {
		values = append(values, unit.Source.ReleaseRef)
	}
	return CompactReleases(values)
}

// Canonical is the binding's one identity document: units and slots are
// sorted, while root and provider order remain part of the accepted binding.
// Equal bindings compare equal, and its Digest is the binding Identity.
func (b BindingSnapshot) Canonical() (framevalue.JSON, error) {
	if err := b.Validate(); err != nil {
		return framevalue.JSON{}, err
	}
	value := b.Clone()
	slices.SortFunc(value.Units, func(a, b UnitBinding) int { return a.Source.compare(b.Source) })
	for i := range value.Units {
		value.Units[i] = value.Units[i].canonical()
	}
	return framevalue.EncodeJSON(value)
}

func (b BindingSnapshot) Identity() (framevalue.Digest, error) {
	canonical, err := b.Canonical()
	return canonical.Digest(), err
}
