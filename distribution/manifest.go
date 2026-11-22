// Package distribution validates immutable plugin releases. It never chooses a
// provider or grants an installation permission to execute.
package distribution

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

const ManifestName = "manifest.json"

var (
	ErrInvalid   = errors.New("distribution: invalid release")
	ErrIntegrity = errors.New("distribution: content integrity failure")
	ErrUntrusted = errors.New("distribution: release was not trusted")
	ErrCapacity  = errors.New("distribution: content limit exceeded")
)

type Artifact struct {
	Path       string            `json:"path"`
	Digest     framevalue.Digest `json:"digest"`
	Size       int64             `json:"size"`
	Executable bool              `json:"executable,omitzero"`
}

type RuntimeReference struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Digest  framevalue.Digest `json:"digest"`
}

type Variant struct {
	OS      string            `json:"os"`
	Arch    string            `json:"arch"`
	Path    string            `json:"path"`
	Runtime *RuntimeReference `json:"runtime,omitempty"`
}

type ModuleImport struct {
	Specifier string               `json:"specifier"`
	Path      string               `json:"path,omitempty"`
	Shared    *HostModuleReference `json:"shared,omitempty"`
}

// HostModuleReference selects a versioned native realm profile. It is not a
// claim about an interpreter binary or portable wire compatibility. The host
// admits each specifier against its concrete implementation of this profile.
type HostModuleReference struct {
	Profile string `json:"profile"`
}

type Module struct {
	Entry   string         `json:"entry"`
	Imports []ModuleImport `json:"imports,omitempty"`
}

type Unit struct {
	ID                    framehost.UnitID              `json:"id"`
	Profile               framehost.Profile             `json:"profile"`
	Imports               []framehost.ImportDeclaration `json:"imports,omitempty"`
	Exports               []framehost.ExportDeclaration `json:"exports,omitempty"`
	Variants              []Variant                     `json:"variants,omitempty"`
	Module                *Module                       `json:"module,omitempty"`
	Resources             []string                      `json:"resources,omitempty"`
	RequestedCapabilities []string                      `json:"requestedCapabilities,omitempty"`
}

type Contribution struct {
	Unit     framehost.UnitID      `json:"unit"`
	Export   string                `json:"export"`
	Point    framehost.ContractRef `json:"point"`
	Key      string                `json:"key"`
	Metadata jsontext.Value        `json:"metadata,omitzero"`
}

// Manifest is a release's one declaration. Its release digest is Identity, a
// derivation the manifest never stores beside itself.
type Manifest struct {
	Package       string                  `json:"package"`
	Version       string                  `json:"version"`
	Artifacts     []Artifact              `json:"artifacts"`
	Contracts     []framehost.ContractRef `json:"contracts,omitempty"`
	Units         []Unit                  `json:"units"`
	Contributions []Contribution          `json:"contributions,omitempty"`
	Dependencies  []Dependency            `json:"dependencies,omitempty"`
}

// Dependency pins another package to its exact release.
type Dependency struct {
	Package string            `json:"package"`
	Release framevalue.Digest `json:"release"`
}

func ParseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := framevalue.DecodeValue(data, &manifest, "manifest"); err != nil {
		return manifest, errors.Join(ErrInvalid, err)
	}
	return manifest, manifest.Validate()
}

func validateDependencies(dependencies []Dependency) error {
	seen := map[string]struct{}{}
	for _, dependency := range dependencies {
		if !frame.ValidName(dependency.Package) || dependency.Release.Validate() != nil {
			return ErrInvalid
		}
		if _, ok := seen[dependency.Package]; ok {
			return ErrInvalid
		}
		seen[dependency.Package] = struct{}{}
	}
	return nil
}

func (m Manifest) Validate() error {
	switch {
	case !frame.ValidName(m.Package):
		return fmt.Errorf("%w: package name %q", ErrInvalid, m.Package)
	case !frame.ValidName(m.Version):
		return fmt.Errorf("%w: version %q", ErrInvalid, m.Version)
	}
	if err := validateDependencies(m.Dependencies); err != nil {
		return err
	}
	artifacts, err := indexArtifacts(m.Artifacts)
	if err != nil {
		return err
	}
	contracts, err := indexContracts(m.Contracts)
	if err != nil {
		return err
	}
	units := make(map[framehost.UnitID]Unit, len(m.Units))
	for _, unit := range m.Units {
		if _, exists := units[unit.ID]; exists {
			return fmt.Errorf("%w: duplicate unit %q", ErrInvalid, unit.ID)
		}
		if err := unit.validate(contracts, artifacts); err != nil {
			return fmt.Errorf("unit %q: %w", unit.ID, err)
		}
		units[unit.ID] = unit
	}
	return validateContributions(m.Contributions, units, contracts)
}

func indexArtifacts(declared []Artifact) (map[string]Artifact, error) {
	artifacts := make(map[string]Artifact, len(declared))
	paths := reservedMetadataPaths()
	for _, artifact := range declared {
		if artifact.Size < 0 || artifact.Digest.Validate() != nil {
			return nil, fmt.Errorf("%w: artifact %q", ErrInvalid, artifact.Path)
		}
		if err := paths.admit(artifact.Path, fileEntry); err != nil {
			return nil, err
		}
		artifacts[artifact.Path] = artifact
	}
	return artifacts, nil
}

func indexContracts(declared []framehost.ContractRef) (map[framehost.ContractRef]struct{}, error) {
	contracts := map[framehost.ContractRef]struct{}{}
	nominal := map[framehost.ContractKey]framehost.ContractRef{}
	for _, contract := range declared {
		if err := contract.Validate(); err != nil {
			return nil, errors.Join(ErrInvalid, err)
		}
		if _, ok := contracts[contract]; ok {
			return nil, fmt.Errorf("%w: duplicate contract %q", ErrInvalid, contract.Name)
		}
		key := contract.Key()
		if prior, ok := nominal[key]; ok && prior != contract {
			return nil, fmt.Errorf("%w: conflicting contract version %q", ErrInvalid, contract.Name)
		}
		nominal[key] = contract
		contracts[contract] = struct{}{}
	}
	return contracts, nil
}

// Declaration is the Host declaration of this unit; the Host owns the
// validity of its identity, slots and exports.
func (u Unit) Declaration() framehost.UnitDeclaration {
	return framehost.UnitDeclaration{ID: u.ID, Profile: u.Profile, Imports: slices.Clone(u.Imports), Exports: slices.Clone(u.Exports)}
}

func (u Unit) validate(contracts map[framehost.ContractRef]struct{}, artifacts map[string]Artifact) error {
	if err := u.Declaration().Validate(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if err := u.validateContracts(contracts); err != nil {
		return err
	}
	if err := u.validateVariants(artifacts); err != nil {
		return err
	}
	if u.Module != nil {
		if err := u.Module.validate(artifacts); err != nil {
			return err
		}
	}
	for _, resource := range u.Resources {
		if _, exists := artifacts[resource]; !exists {
			return fmt.Errorf("%w: resource path %q", ErrInvalid, resource)
		}
	}
	for _, capability := range u.RequestedCapabilities {
		if !frame.ValidName(capability) {
			return fmt.Errorf("%w: capability name %q", ErrInvalid, capability)
		}
	}
	_, err := u.profileShape()
	return err
}

// validateContracts requires every slot and export to name a contract the
// manifest itself publishes.
func (u Unit) validateContracts(contracts map[framehost.ContractRef]struct{}) error {
	for _, imported := range u.Imports {
		if _, exists := contracts[imported.Contract]; !exists {
			return fmt.Errorf("%w: import slot %q names an undeclared contract", ErrInvalid, imported.Slot)
		}
	}
	for _, export := range u.Exports {
		if _, exists := contracts[export.Contract]; !exists {
			return fmt.Errorf("%w: export %q names an undeclared contract", ErrInvalid, export.Name)
		}
	}
	return nil
}

func (u Unit) validateVariants(artifacts map[string]Artifact) error {
	targets := map[[2]string]struct{}{}
	for _, variant := range u.Variants {
		artifact, exists := artifacts[variant.Path]
		if !exists || !frame.ValidName(variant.OS) || !frame.ValidName(variant.Arch) {
			return fmt.Errorf("%w: unit artifact %q", ErrInvalid, variant.Path)
		}
		if variant.Runtime == nil && !artifact.Executable {
			return fmt.Errorf("%w: process entry %q is not executable", ErrInvalid, variant.Path)
		}
		if variant.Runtime != nil && !variant.Runtime.valid() {
			return fmt.Errorf("%w: runtime reference of %q", ErrInvalid, variant.Path)
		}
		key := [2]string{variant.OS, variant.Arch}
		if _, exists := targets[key]; exists {
			return fmt.Errorf("%w: duplicate target variant %s/%s", ErrInvalid, variant.OS, variant.Arch)
		}
		targets[key] = struct{}{}
	}
	return nil
}

func (m Module) validate(artifacts map[string]Artifact) error {
	if _, exists := artifacts[m.Entry]; !exists {
		return fmt.Errorf("%w: module entry %q", ErrInvalid, m.Entry)
	}
	specifiers := map[string]struct{}{}
	for _, imported := range m.Imports {
		if !frame.ValidName(imported.Specifier) || (imported.Path == "") == (imported.Shared == nil) {
			return fmt.Errorf("%w: module import %q", ErrInvalid, imported.Specifier)
		}
		if _, exists := specifiers[imported.Specifier]; exists {
			return fmt.Errorf("%w: duplicate module import %q", ErrInvalid, imported.Specifier)
		}
		specifiers[imported.Specifier] = struct{}{}
		if imported.Shared != nil {
			if !frame.ValidName(imported.Shared.Profile) {
				return fmt.Errorf("%w: shared module %q", ErrInvalid, imported.Specifier)
			}
		} else if _, exists := artifacts[imported.Path]; !exists {
			return fmt.Errorf("%w: module path %q", ErrInvalid, imported.Path)
		}
	}
	return nil
}

func (u Unit) ValidateProcessEntry() error {
	if len(u.Variants) == 0 || u.Module != nil {
		return fmt.Errorf("%w: process unit entry", ErrInvalid)
	}
	if err := u.Declaration().RequirePortable(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	return nil
}

func (u Unit) profileShape() (bool, error) {
	switch u.Profile {
	case framehost.LocalProcess:
		return true, u.ValidateProcessEntry()
	case framehost.BrowserJS:
		if u.Module == nil || len(u.Variants) != 0 {
			return true, fmt.Errorf("%w: browser unit entry", ErrInvalid)
		}
	case framehost.StaticResource:
		for _, export := range u.Exports {
			if export.Contract.Mode != framehost.NativeMode {
				return true, fmt.Errorf("%w: static resource export %q must be native", ErrInvalid, export.Name)
			}
		}
		if len(u.Resources) == 0 || u.Module != nil || len(u.Variants) != 0 || len(u.Imports) != 0 {
			return true, fmt.Errorf("%w: resource unit entry", ErrInvalid)
		}
	case framehost.NativeGo:
		if u.Module != nil || len(u.Variants) != 0 {
			return true, fmt.Errorf("%w: native unit is supplied by the host build", ErrInvalid)
		}
	default:
		// Unknown profiles remain inspectable. Installation requires a registered validator.
		return false, nil
	}
	return true, nil
}

func validateContributions(declared []Contribution, units map[framehost.UnitID]Unit, contracts map[framehost.ContractRef]struct{}) error {
	type contributionKey struct {
		unit       framehost.UnitID
		point, key string
	}
	keys := map[contributionKey]struct{}{}
	type resourceSlot struct {
		unit   framehost.UnitID
		export string
	}
	resourceExports := map[resourceSlot]bool{}
	for _, contribution := range declared {
		unit, ok := units[contribution.Unit]
		if !ok || !frame.ValidName(contribution.Key) || !frame.ValidName(contribution.Export) {
			return fmt.Errorf("%w: contribution %q of unit %q", ErrInvalid, contribution.Key, contribution.Unit)
		}
		if _, ok := contracts[contribution.Point]; !ok || contribution.Point.Kind != framehost.PointKind {
			return fmt.Errorf("%w: contribution %q point", ErrInvalid, contribution.Key)
		}
		if !slices.ContainsFunc(unit.Exports, func(export framehost.ExportDeclaration) bool {
			return export.Name == contribution.Export && export.Contract == contribution.Point
		}) {
			return fmt.Errorf("%w: contribution %q export %q", ErrInvalid, contribution.Key, contribution.Export)
		}
		if unit.Profile == framehost.StaticResource {
			slot := resourceSlot{unit.ID, contribution.Export}
			if len(contribution.Metadata) == 0 || resourceExports[slot] {
				return fmt.Errorf("%w: static resource contribution %q requires one metadata value", ErrInvalid, contribution.Export)
			}
			resourceExports[slot] = true
		}
		key := contributionKey{contribution.Unit, contribution.Point.Name, contribution.Key}
		if _, ok := keys[key]; ok {
			return fmt.Errorf("%w: duplicate contribution %q", ErrInvalid, contribution.Key)
		}
		keys[key] = struct{}{}
		if len(contribution.Metadata) != 0 {
			if _, err := framevalue.ParseJSON(contribution.Metadata); err != nil {
				return errors.Join(ErrInvalid, err)
			}
		}
	}
	return nil
}

// Canonical preserves all declaration order. Artifact order is a set and is
// normalized; metadata is JSON syntax, not an inferred schema equivalence.
func (m Manifest) Canonical() (framevalue.JSON, error) {
	if err := m.Validate(); err != nil {
		return framevalue.JSON{}, err
	}
	m.Artifacts = sortedArtifacts(m.Artifacts)
	return framevalue.EncodeJSON(m)
}

// Identity is the release digest: the canonical manifest's, covering the
// declaration and its artifact inventory.
func (m Manifest) Identity() (framevalue.Digest, error) {
	canonical, err := m.Canonical()
	return canonical.Digest(), err
}

func sortedArtifacts(artifacts []Artifact) []Artifact {
	return slices.SortedFunc(slices.Values(artifacts), func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
}

func (r RuntimeReference) valid() bool {
	return frame.ValidName(r.Name) && frame.ValidName(r.Version) && r.Digest.Validate() == nil
}
