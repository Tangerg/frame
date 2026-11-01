package frame

import (
	"cmp"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"

	"github.com/Tangerg/frame/integrity"
)

type Dependency struct {
	InstallationID InstallationID   `json:"installationId"`
	Release        integrity.Digest `json:"release"`
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
	InstallationID InstallationID   `json:"installationId"`
	Release        integrity.Digest `json:"release"`
	UnitID         UnitID           `json:"unitId"`
}

func (r UnitRef) Validate() error {
	if err := (Dependency{InstallationID: r.InstallationID, Release: r.Release}).Validate(); err != nil {
		return err
	}
	return r.UnitID.Validate()
}

func (r UnitRef) Dependency() Dependency {
	return Dependency{InstallationID: r.InstallationID, Release: r.Release}
}

type ExportRef struct {
	InstallationID InstallationID   `json:"installationId"`
	Release        integrity.Digest `json:"release"`
	UnitID         UnitID           `json:"unitId"`
	Export         string           `json:"export"`
	Contract       ContractRef      `json:"contract"`
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
		if err := validateName("import slot", value.Slot); err != nil {
			return err
		}
		if seen[value.Slot] {
			return fmt.Errorf("%w: duplicate import slot %s", ErrInvalid, value.Slot)
		}
		seen[value.Slot] = true
		if err := value.Contract.Validate(); err != nil {
			return err
		}
		for _, provider := range value.Providers {
			if err := provider.Validate(); err != nil {
				return err
			}
			if provider.Contract != value.Contract {
				return ErrContractMismatch
			}
		}
	}
	return nil
}

type UnitBinding struct {
	Source        UnitRef             `json:"source"`
	Configuration integrity.Digest    `json:"configuration"`
	Imports       []ImportDeclaration `json:"imports"`
}

func (b UnitBinding) Clone() UnitBinding {
	b.Imports = slices.Clone(b.Imports)
	for i := range b.Imports {
		b.Imports[i] = b.Imports[i].Clone()
	}
	return b
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

func (b BindingSnapshot) Validate() error {
	if b.FormatVersion != 1 || b.Roots == nil || b.Units == nil {
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
		if err := validateName("import slot", binding.Slot); err != nil {
			return err
		}
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

// validateBound checks a resolved import: its providers satisfy the contract's
// cardinality and each provides exactly that contract.
func (d ImportDeclaration) validateBound() error {
	if err := d.Contract.Validate(); err != nil {
		return err
	}
	if d.Providers == nil || (!d.Optional && len(d.Providers) == 0) || (d.Contract.Kind == ServiceKind && len(d.Providers) > 1) {
		return fmt.Errorf("%w: bound import cardinality", ErrInvalid)
	}
	providers := make(map[ExportRef]bool)
	for _, provider := range d.Providers {
		if err := provider.Validate(); err != nil {
			return err
		}
		if provider.Contract != d.Contract {
			return ErrContractMismatch
		}
		if providers[provider] {
			return fmt.Errorf("%w: duplicate bound provider", ErrInvalid)
		}
		providers[provider] = true
	}
	return nil
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
	slices.SortFunc(value.Units, func(a, b UnitBinding) int { return compareUnit(a.Source, b.Source) })
	for i := range value.Units {
		slices.SortFunc(value.Units[i].Imports, func(a, b ImportDeclaration) int { return cmp.Compare(a.Slot, b.Slot) })
	}
	return json.Marshal(value)
}

func (b BindingSnapshot) Identity() (integrity.Digest, error) {
	data, err := b.CanonicalJSON()
	if err != nil {
		return integrity.Digest{}, err
	}
	return integrity.Hash(data), nil
}
