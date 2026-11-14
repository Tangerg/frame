package host

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// Driver materializes one approved unit. Its exports and resources remain
// owned by Setup's instance; it cannot choose replacements for imports.
type Driver interface {
	Profile() Profile
	Prepare(context.Context, Setup) ([]ExportValue, error)
}

// ExportValidator admits a driver's host-side representation when its execution
// realm cannot expose the contract's local Go implementation. The driver must
// validate the value against the original instance and export identity.
type ExportValidator interface {
	ValidateExport(instanceID string, export ExportValue) error
}

// RealmDriver creates separate actual instances for each explicitly addressed
// execution realm. Provider selection remains with Host.
type RealmDriver interface {
	Driver
	RealmBound() bool
}

type registeredDriver struct {
	Driver
	realmBound bool
}

type SetupFunc func(context.Context, Setup) ([]ExportValue, error)

type ExportValue struct {
	Name     string
	Contract ContractRef
	Metadata any
	Value    any
}

type BoundValue struct {
	Source ExportRef
	Value  any
	// Local identifies an exact export of this same approved Unit. A driver
	// resolves it inside its own prepared execution; it is never a nil proxy.
	Local bool
}

// Setup is one preparation of an unitInstance. It ends when the preparation
// returns; an activation callback registered during it runs after admission.
type Setup struct{ *setupCall }

// Every handle shares one result and one seal, including callback interruption
// or replacement of the public handle. Sealing never waits for Host state.
type setupCall struct {
	host     *hostState
	instance *unitInstance
	work     frame.Work
	imports  map[string][]BoundValue

	mu       sync.Mutex
	sealed   bool
	exports  []ExportValue
	activate func(context.Context) error
}

func (s Setup) Unit() UnitDeclaration     { return s.instance.unit.Clone() }
func (s Setup) Binding() UnitBinding      { return s.instance.binding.Clone() }
func (s Setup) Snapshot() BindingSnapshot { return s.instance.snapshot.Clone() }
func (s Setup) Configuration() []byte     { return bytes.Clone(s.instance.configuration) }
func (s Setup) InstanceID() string        { return s.instance.id }
func (s Setup) Generation() uint64        { return s.instance.generation }
func (s Setup) Epoch() uint64             { return s.host.Epoch() }
func (s Setup) ScopeGrant() ScopeGrant    { return s.instance.grant() }
func (s Setup) Realm() RealmID            { return s.instance.realm }

// Valid reports whether the handle names a preparation; the zero Setup does not.
func (s Setup) Valid() bool { return s.setupCall != nil }

func (s Setup) Lifetime() frame.Lifetime {
	if s.setupCall == nil {
		return frame.Lifetime{}
	}
	return s.instance.lifetime
}

func (s Setup) Context() context.Context {
	if s.setupCall == nil {
		return nil
	}
	return s.work.Context()
}

func (s Setup) ValueContract(ref ContractRef) (ContractRef, error) {
	return s.host.ValueContract(ref)
}

func (s Setup) ContractDescriptor(ref ContractRef) ([]byte, error) {
	return s.host.ContractDescriptor(ref)
}

func (s Setup) Capability(ref ContractRef) (*frame.Capability, error) {
	return s.host.Capability(ref)
}

func (s Setup) Own(resource frame.Resource, dependencies ...frame.ResourceHandle) (frame.ResourceHandle, error) {
	if s.setupCall == nil {
		return frame.ResourceHandle{}, frame.ErrInvalid
	}
	return s.work.Own(resource, dependencies...)
}
func (s Setup) Cleanup(name string, closeResource func() error, dependencies ...frame.ResourceHandle) (frame.ResourceHandle, error) {
	if s.setupCall == nil {
		return frame.ResourceHandle{}, frame.ErrInvalid
	}
	return s.work.Cleanup(name, closeResource, dependencies...)
}

func (h *hostState) OwnsSetup(s Setup) bool {
	return h != nil && s.setupCall != nil && s.host == h
}

// ValidateCurrent checks the original instance and frozen binding together,
// before a driver exposes a previously prepared execution to its realm.
func (s Setup) ValidateCurrent(ctx context.Context) error {
	if s.setupCall == nil {
		return frame.ErrInvalid
	}
	return s.instance.validateCurrent(ctx)
}

func (s Setup) OnActivate(activate func(context.Context) error) error {
	if s.setupCall == nil || activate == nil {
		return frame.ErrInvalid
	}
	call := s.setupCall
	call.host.mu.Lock()
	defer call.host.mu.Unlock()
	call.mu.Lock()
	defer call.mu.Unlock()
	if call.sealed || !call.instance.isCurrentLocked() {
		return frame.ErrClosed
	}
	if call.activate != nil {
		return frame.ErrInvalid
	}
	call.activate = activate
	return nil
}

func (c *setupCall) seal(exports []ExportValue) ([]ExportValue, func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sealed = true
	exports = append(exports, c.exports...)
	activate := c.activate
	c.exports = nil
	c.activate = nil
	return exports, activate
}

func (s Setup) Imports(slot string) ([]BoundValue, error) {
	if s.setupCall == nil {
		return nil, frame.ErrInvalid
	}
	values, found := s.imports[slot]
	if !found {
		return nil, fmt.Errorf("%w: undeclared import slot %s", frame.ErrInvalid, slot)
	}
	return slices.Clone(values), nil
}

// readImport returns the single value bound to the import the author expects.
// An optional import without a provider reports found=false.
func readImport[T any](setup Setup, expected ImportDeclaration) (value T, found bool, err error) {
	if setup.setupCall == nil || expected.Contract == (ContractRef{}) {
		return value, false, frame.ErrInvalid
	}
	declared, exists := setup.instance.unit.importSlot(expected.Slot)
	if !exists {
		return value, false, fmt.Errorf("%w: undeclared import slot %q", frame.ErrInvalid, expected.Slot)
	}
	if declared.Contract != expected.Contract || declared.Optional != expected.Optional {
		return value, false, frame.ErrContractMismatch
	}
	values := setup.imports[expected.Slot]
	switch {
	case len(values) == 0 && expected.Optional:
		return value, false, nil
	case len(values) == 0:
		return value, false, ErrUnavailable
	case len(values) != 1 || values[0].Source.Contract != declared.Contract:
		return value, false, frame.ErrContractMismatch
	case values[0].Local:
		return value, false, ErrUnavailable
	}
	value, ok := values[0].Value.(T)
	if !ok {
		return value, false, frame.ErrContractMismatch
	}
	return value, true, nil
}

func Import[T any](setup Setup, slot string, contract Contract[T]) (T, error) {
	value, _, err := readImport[T](setup, ImportDeclaration{Slot: slot, Contract: contract.Ref()})
	return value, err
}

func OptionalImport[T any](setup Setup, slot string, contract Contract[T]) (T, bool, error) {
	return readImport[T](setup, ImportDeclaration{Slot: slot, Contract: contract.Ref(), Optional: true})
}

func Provide[T any](name string, contract Contract[T], value T) ExportValue {
	return ExportValue{Name: name, Contract: contract.Ref(), Value: value}
}

func Contribute[M, V any](name string, point Point[M, V], metadata M, value V) ExportValue {
	return ExportValue{Name: name, Contract: point.Ref(), Metadata: metadata, Value: value}
}

type Unit struct {
	Declaration UnitDeclaration
	Setup       SetupFunc
}

type Definition struct {
	Revision framevalue.Digest
	Units    []Unit
}

// Plugin is a reusable declaration. Host installation operations capture it,
// so replacing the caller's handle cannot change an admitted release.
// Constructing a declaration never starts code.
type Plugin struct {
	revision framevalue.Digest
	units    []Unit
}

func Define(definition Definition) (*Plugin, error) {
	if err := definition.Revision.Validate(); err != nil {
		return nil, err
	}
	plugin := &Plugin{revision: definition.Revision, units: slices.Clone(definition.Units)}
	seen := make(map[UnitID]bool)
	for i, unit := range plugin.units {
		if err := unit.Declaration.Validate(); err != nil {
			return nil, err
		}
		if seen[unit.Declaration.ID] {
			return nil, fmt.Errorf("%w: duplicate unit", frame.ErrInvalid)
		}
		seen[unit.Declaration.ID] = true
		if unit.Declaration.Profile == NativeGo && unit.Setup == nil {
			return nil, fmt.Errorf("%w: native unit requires setup", frame.ErrInvalid)
		}
		if unit.Declaration.Profile != NativeGo && unit.Setup != nil {
			return nil, fmt.Errorf("%w: non-native unit cannot supply native setup", frame.ErrInvalid)
		}
		plugin.units[i].Declaration = unit.Declaration.Clone()
	}
	return plugin, nil
}

func (p *Plugin) Revision() framevalue.Digest { return p.revision }
func (p *Plugin) Units() []UnitDeclaration {
	result := make([]UnitDeclaration, len(p.units))
	for i, unit := range p.units {
		result[i] = unit.Declaration.Clone()
	}
	return result
}
