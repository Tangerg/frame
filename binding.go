package frame

import (
	"cmp"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"

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
		return fmt.Errorf("%w: dependency release: %w", ErrInvalid, err)
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

type ImportDeclaration struct {
	Slot      string      `json:"slot"`
	Contract  ContractRef `json:"contract"`
	Optional  bool        `json:"optional"`
	Providers []ExportRef `json:"providers"`
}

func (d ImportDeclaration) Clone() ImportDeclaration {
	d.Providers = slices.Clone(d.Providers)
	return d
}

// equal distinguishes unresolved providers (nil) from a resolved empty set.
func (d ImportDeclaration) equal(other ImportDeclaration) bool {
	return d.Slot == other.Slot && d.Contract == other.Contract && d.Optional == other.Optional &&
		(d.Providers == nil) == (other.Providers == nil) && slices.Equal(d.Providers, other.Providers)
}

func (d ImportDeclaration) Validate() error {
	if err := validateName("import slot", d.Slot); err != nil {
		return err
	}
	if err := d.Contract.Validate(); err != nil {
		return err
	}
	if d.Providers == nil {
		return nil
	}
	return d.validateProviders()
}

func (d ImportDeclaration) validateProviders() error {
	if !d.Optional && len(d.Providers) == 0 || d.Contract.Kind == ServiceKind && len(d.Providers) > 1 {
		return fmt.Errorf("%w: import cardinality", ErrInvalid)
	}
	providers := make(map[ExportRef]bool, len(d.Providers))
	for _, provider := range d.Providers {
		if err := provider.Validate(); err != nil {
			return err
		}
		if provider.Contract != d.Contract {
			return ErrContractMismatch
		}
		if providers[provider] {
			return fmt.Errorf("%w: duplicate import provider", ErrInvalid)
		}
		providers[provider] = true
	}
	return nil
}

type ExportDeclaration struct {
	Name     string      `json:"name"`
	Contract ContractRef `json:"contract"`
}

type UnitDeclaration struct {
	ID      UnitID              `json:"id"`
	Profile Profile             `json:"profile"`
	Imports []ImportDeclaration `json:"imports"`
	Exports []ExportDeclaration `json:"exports"`
}

func (d UnitDeclaration) Clone() UnitDeclaration {
	d.Imports = slices.Clone(d.Imports)
	for i := range d.Imports {
		d.Imports[i] = d.Imports[i].Clone()
	}
	d.Exports = slices.Clone(d.Exports)
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

func (d UnitDeclaration) equal(other UnitDeclaration) bool {
	return d.ID == other.ID && d.Profile == other.Profile &&
		slices.EqualFunc(d.Imports, other.Imports, ImportDeclaration.equal) && slices.Equal(d.Exports, other.Exports)
}

func (d UnitDeclaration) Validate() error {
	if err := d.ID.Validate(); err != nil {
		return err
	}
	if err := d.Profile.Validate(); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, value := range d.Exports {
		if err := validateName("export name", value.Name); err != nil {
			return err
		}
		if seen[value.Name] {
			return fmt.Errorf("%w: duplicate export %s", ErrInvalid, value.Name)
		}
		seen[value.Name] = true
		if err := value.Contract.Validate(); err != nil {
			return err
		}
	}
	seen = make(map[string]bool)
	for _, value := range d.Imports {
		if err := value.Validate(); err != nil {
			return err
		}
		if seen[value.Slot] {
			return fmt.Errorf("%w: duplicate import slot %s", ErrInvalid, value.Slot)
		}
		seen[value.Slot] = true
	}
	return nil
}

type UnitBinding struct {
	Source        UnitRef             `json:"source"`
	Configuration framevalue.Digest   `json:"configuration"`
	Imports       []ImportDeclaration `json:"imports"`
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
	if b.Imports == nil {
		b.Imports = []ImportDeclaration{}
	}
	slices.SortFunc(b.Imports, func(a, b ImportDeclaration) int { return cmp.Compare(a.Slot, b.Slot) })
	return b
}

func (b UnitBinding) equal(other UnitBinding) bool {
	left, right := b.canonical(), other.canonical()
	return left.Source == right.Source && left.Configuration == right.Configuration && slices.EqualFunc(left.Imports, right.Imports, ImportDeclaration.equal)
}

// BindingSnapshot is immutable routing evidence when retained by its domain
// owner. It contains no running pointer, publication grant or business outcome.
type BindingSnapshot struct {
	FormatVersion uint32        `json:"formatVersion"`
	Roots         []ExportRef   `json:"roots"`
	UnitRoots     []UnitRef     `json:"unitRoots"`
	Units         []UnitBinding `json:"units"`
	Rule          string        `json:"rule"`
}

// ParseBinding decodes retained routing evidence without selecting providers.
// The original owner must still validate it under admission before a new use.
func ParseBinding(data []byte) (BindingSnapshot, error) {
	var binding BindingSnapshot
	if err := jsonv2.Unmarshal(data, &binding, jsonv2.RejectUnknownMembers(true)); err != nil {
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

const bindingFormatVersion = 1

func newBindingSnapshot(roots []ExportRef, unitRoots []UnitRef, units []UnitBinding) BindingSnapshot {
	if roots == nil {
		roots = []ExportRef{}
	}
	if units == nil {
		units = []UnitBinding{}
	}
	return BindingSnapshot{FormatVersion: bindingFormatVersion, Roots: slices.Clone(roots), UnitRoots: slices.Clone(unitRoots), Units: units}
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
	if b.FormatVersion != bindingFormatVersion || b.Roots == nil || b.Units == nil {
		return fmt.Errorf("%w: binding format or missing arrays", ErrInvalid)
	}
	if b.Rule != "" {
		if err := validateName("selection rule", b.Rule); err != nil {
			return err
		}
	}
	units := make(map[UnitRef]UnitBinding, len(b.Units))
	for _, unit := range b.Units {
		if err := unit.validate(); err != nil {
			return err
		}
		if _, found := units[unit.Source]; found {
			return fmt.Errorf("%w: duplicate bound unit", ErrInvalid)
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
			return fmt.Errorf("%w: duplicate bound import", ErrInvalid)
		}
		slots[binding.Slot] = true
		if err := binding.validateBound(); err != nil {
			return err
		}
	}
	return nil
}

func (d ImportDeclaration) validateBound() error {
	if d.Providers == nil {
		return fmt.Errorf("%w: unresolved import", ErrInvalid)
	}
	return d.Validate()
}

func (b BindingSnapshot) validateRoots(units map[UnitRef]UnitBinding) error {
	for _, root := range b.Roots {
		if err := root.Validate(); err != nil {
			return err
		}
		if _, exists := units[root.Unit()]; !exists {
			return fmt.Errorf("%w: root unit missing from binding", ErrInvalid)
		}
	}
	unitRoots := make(map[UnitRef]bool, len(b.UnitRoots))
	for _, root := range b.UnitRoots {
		if err := root.Validate(); err != nil {
			return err
		}
		if _, exists := units[root]; !exists {
			return fmt.Errorf("%w: root unit missing from binding", ErrInvalid)
		}
		if unitRoots[root] {
			return fmt.Errorf("%w: duplicate root unit", ErrInvalid)
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
			return fmt.Errorf("%w: bound provider unit missing", ErrInvalid)
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
		return fmt.Errorf("%w: binding includes unreachable units", ErrInvalid)
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

func (b BindingSnapshot) CanonicalJSON() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	value := b.Clone()
	if value.UnitRoots == nil {
		value.UnitRoots = []UnitRef{}
	}
	slices.SortFunc(value.Units, func(a, b UnitBinding) int { return compareUnit(a.Source, b.Source) })
	for i := range value.Units {
		value.Units[i] = value.Units[i].canonical()
	}
	return json.Marshal(value)
}

func (b BindingSnapshot) Identity() (framevalue.Digest, error) {
	data, err := b.CanonicalJSON()
	if err != nil {
		return framevalue.Digest{}, err
	}
	return framevalue.Hash(data), nil
}
