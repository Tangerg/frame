// Package distribution validates immutable plugin releases. It never chooses a
// provider or grants an installation permission to execute.
package distribution

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

const ManifestName = "manifest.json"
const LockName = "plugin.lock.json"

const ManifestSchemaVersion = 1
const LockSchemaVersion = 1

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
	Executable bool              `json:"executable,omitempty"`
}

// An import is still a declaration here. Installation and binding supply its
// exact provider references; a package cannot name a future InstallationID.
type Import struct {
	Slot     string                `json:"slot"`
	Contract framehost.ContractRef `json:"contract"`
	Optional bool                  `json:"optional,omitempty"`
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
	Imports               []Import                      `json:"imports,omitempty"`
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
	Metadata json.RawMessage       `json:"metadata,omitzero"`
}

type Manifest struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Package       string                  `json:"package"`
	Version       string                  `json:"version"`
	Release       framevalue.Digest       `json:"release,omitempty"`
	Artifacts     []Artifact              `json:"artifacts"`
	Contracts     []framehost.ContractRef `json:"contracts,omitempty"`
	Units         []Unit                  `json:"units"`
	Contributions []Contribution          `json:"contributions,omitempty"`
	Dependencies  []LockedRelease         `json:"dependencies,omitempty"`
}

type LockedRelease struct {
	Package string            `json:"package"`
	Release framevalue.Digest `json:"release"`
}

type Lock struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Package       string                  `json:"package"`
	Release       framevalue.Digest       `json:"release"`
	Artifacts     []Artifact              `json:"artifacts"`
	Contracts     []framehost.ContractRef `json:"contracts,omitempty"`
	Dependencies  []LockedRelease         `json:"dependencies,omitempty"`
}

func ParseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := jsonv2.Unmarshal(data, &manifest, jsonv2.RejectUnknownMembers(true)); err != nil {
		return manifest, errors.Join(ErrInvalid, err)
	}
	return manifest, manifest.Validate()
}

func ParseLock(data []byte) (Lock, error) {
	var lock Lock
	if err := jsonv2.Unmarshal(data, &lock, jsonv2.RejectUnknownMembers(true)); err != nil {
		return lock, errors.Join(ErrInvalid, err)
	}
	return lock, lock.Validate()
}

func (l Lock) Validate() error {
	if l.SchemaVersion != LockSchemaVersion || !frame.ValidName(l.Package) || l.Release.Validate() != nil {
		return ErrInvalid
	}
	if err := validateDependencies(l.Dependencies); err != nil {
		return err
	}
	if _, err := indexArtifacts(l.Artifacts); err != nil {
		return err
	}
	_, err := indexContracts(l.Contracts)
	return err
}

func validateDependencies(dependencies []LockedRelease) error {
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
	case m.SchemaVersion != ManifestSchemaVersion:
		return fmt.Errorf("%w: schema version %d", ErrInvalid, m.SchemaVersion)
	case !frame.ValidName(m.Package):
		return fmt.Errorf("%w: package name %q", ErrInvalid, m.Package)
	case !frame.ValidName(m.Version):
		return fmt.Errorf("%w: version %q", ErrInvalid, m.Version)
	case !m.Release.IsZero() && m.Release.Validate() != nil:
		return fmt.Errorf("%w: release digest", ErrInvalid)
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

func (u Unit) validate(contracts map[framehost.ContractRef]struct{}, artifacts map[string]Artifact) error {
	if u.ID.Validate() != nil || u.Profile.Validate() != nil {
		return fmt.Errorf("%w: unit identity", ErrInvalid)
	}
	if err := u.validateBindings(contracts); err != nil {
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

func (u Unit) validateBindings(contracts map[framehost.ContractRef]struct{}) error {
	slots := map[string]struct{}{}
	for _, imported := range u.Imports {
		if !frame.ValidName(imported.Slot) {
			return fmt.Errorf("%w: import slot %q", ErrInvalid, imported.Slot)
		}
		if _, exists := slots[imported.Slot]; exists {
			return fmt.Errorf("%w: duplicate import slot %q", ErrInvalid, imported.Slot)
		}
		if _, exists := contracts[imported.Contract]; !exists {
			return fmt.Errorf("%w: import slot %q names an undeclared contract", ErrInvalid, imported.Slot)
		}
		slots[imported.Slot] = struct{}{}
	}
	exports := map[string]struct{}{}
	for _, export := range u.Exports {
		if !frame.ValidName(export.Name) {
			return fmt.Errorf("%w: export name %q", ErrInvalid, export.Name)
		}
		if _, exists := exports[export.Name]; exists {
			return fmt.Errorf("%w: duplicate export %q", ErrInvalid, export.Name)
		}
		if _, exists := contracts[export.Contract]; !exists {
			return fmt.Errorf("%w: export %q names an undeclared contract", ErrInvalid, export.Name)
		}
		exports[export.Name] = struct{}{}
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
		if variant.Runtime != nil && !validRuntime(*variant.Runtime) {
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
	for _, imported := range u.Imports {
		if imported.Contract.Mode != framehost.PortableMode {
			return fmt.Errorf("%w: process import %q is native", ErrInvalid, imported.Slot)
		}
	}
	for _, exported := range u.Exports {
		if exported.Contract.Mode != framehost.PortableMode {
			return fmt.Errorf("%w: process export %q is native", ErrInvalid, exported.Name)
		}
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
func (m Manifest) Canonical() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	m.Release = framevalue.Digest{}
	m.Artifacts = slices.Clone(m.Artifacts)
	slices.SortFunc(m.Artifacts, func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	canonical, err := framevalue.ParseJSON(encoded)
	if err != nil {
		return nil, err
	}
	return canonical.Bytes(), nil
}

func (m Manifest) Identity() (framevalue.Digest, error) {
	data, err := m.Canonical()
	if err != nil {
		return framevalue.Digest{}, err
	}
	return framevalue.Hash(data), nil
}

func (m Manifest) Lock() (Lock, error) {
	digest, err := m.Identity()
	if err != nil {
		return Lock{}, err
	}
	lock := Lock{SchemaVersion: LockSchemaVersion, Package: m.Package, Release: digest, Artifacts: slices.Clone(m.Artifacts), Contracts: slices.Clone(m.Contracts), Dependencies: slices.Clone(m.Dependencies)}
	slices.SortFunc(lock.Artifacts, func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
	return lock, nil
}

func (l Lock) Verify(m Manifest) error {
	expected, err := m.Lock()
	if err != nil {
		return err
	}
	l.Artifacts = slices.Clone(l.Artifacts)
	slices.SortFunc(l.Artifacts, func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(l)
	if !bytes.Equal(a, b) {
		return errors.Join(ErrIntegrity, errors.New("lock does not describe the release"))
	}
	return nil
}

func validRuntime(value RuntimeReference) bool {
	return frame.ValidName(value.Name) && frame.ValidName(value.Version) && value.Digest.Validate() == nil
}

// Portable archive paths deliberately use ASCII. This rejects Unicode and
// case-fold aliases rather than depending on a destination filesystem's rules.
const maxArtifactPathBytes = 1024
const maxArtifactPathSegmentBytes = 240

// Release paths must stay valid on every platform a host may extract to, so
// Windows device names and reserved characters are refused everywhere.
var (
	windowsDeviceNames            = []string{"con", "prn", "aux", "nul"}
	windowsNumberedDevicePrefixes = []string{"com", "lpt"}
)

const windowsReservedCharacters = ":*?\"<>|"

func validPath(value string) bool {
	if value == "" || len(value) > maxArtifactPathBytes || !fs.ValidPath(value) || value == "." || strings.Contains(value, "\\") {
		return false
	}
	if strings.ContainsFunc(value, unportableRune) {
		return false
	}
	return !slices.ContainsFunc(strings.Split(value, "/"), unportableSegment)
}

func unportableRune(r rune) bool {
	return r < '!' || r > '~' || strings.ContainsRune(windowsReservedCharacters, r)
}

func unportableSegment(segment string) bool {
	if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") || len(segment) > maxArtifactPathSegmentBytes {
		return true
	}
	stem := strings.ToLower(strings.SplitN(segment, ".", 2)[0])
	return slices.Contains(windowsDeviceNames, stem) || numberedDevice(stem)
}

func numberedDevice(stem string) bool {
	if len(stem) != 4 || stem[3] < '1' || stem[3] > '9' {
		return false
	}
	return slices.Contains(windowsNumberedDevicePrefixes, stem[:3])
}

func isReleaseMetadata(name string) bool { return name == ManifestName || name == LockName }

type entryKind uint8

const (
	fileEntry entryKind = iota
	directoryEntry
	ancestorEntry
)

// Implicit ancestors reserve their original spelling before a later explicit
// directory can introduce a filesystem-dependent alias.
type pathEntry struct {
	name string
	kind entryKind
}

type pathInventory map[string]pathEntry

func reservedMetadataPaths() pathInventory {
	return pathInventory{ManifestName: {name: ManifestName, kind: fileEntry}, LockName: {name: LockName, kind: fileEntry}}
}

func (paths pathInventory) ownsFile(name string) bool {
	entry, present := paths[strings.ToLower(name)]
	return present && entry.name == name && entry.kind == fileEntry
}

func (paths pathInventory) admitBounded(name string, kind entryKind, limit int) error {
	if err := paths.admit(name, kind); err != nil {
		return err
	}
	if len(paths) > limit {
		return ErrCapacity
	}
	return nil
}

func (paths pathInventory) admit(name string, kind entryKind) error {
	if !validPath(name) {
		return fmt.Errorf("%w: unsafe path", ErrInvalid)
	}
	key := strings.ToLower(name)
	if prior, exists := paths[key]; exists && (prior.kind != ancestorEntry || kind != directoryEntry || prior.name != name) {
		return fmt.Errorf("%w: duplicate or aliased path", ErrInvalid)
	}
	for ancestor := path.Dir(name); ancestor != "."; ancestor = path.Dir(ancestor) {
		key := strings.ToLower(ancestor)
		if prior, ok := paths[key]; ok {
			if prior.kind == fileEntry || prior.name != ancestor {
				return fmt.Errorf("%w: file or alias is a path ancestor", ErrInvalid)
			}
		} else {
			paths[key] = pathEntry{name: ancestor, kind: ancestorEntry}
		}
	}
	paths[key] = pathEntry{name: name, kind: kind}
	return nil
}
