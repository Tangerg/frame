package frame

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	framevalue "github.com/Tangerg/frame/value"
)

// Driver materializes one approved unit. Its exports and resources remain
// owned by Setup's instance; it cannot choose replacements for imports.
type Driver interface {
	Profile() Profile
	Prepare(context.Context, *Setup) ([]ExportValue, error)
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

type SetupFunc func(context.Context, *Setup) ([]ExportValue, error)

type ExportValue struct {
	Name     string
	Contract ContractRef
	Metadata any
	Value    any
	Durable  bool
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
type Setup struct {
	host     *Host
	instance *unitInstance
	work     *Work
	imports  map[string][]BoundValue
	staged   *stagedExports
	// Setup is a handle: copies share the same activation registration.
	activation *activation
}

// activation is the callback a setup registered to run after admission. Host.mu
// guards it; sealing ends registration when the preparation returns.
type activation struct {
	callback func(context.Context) error
	sealed   bool
}

func (s *Setup) Unit() UnitDeclaration     { return s.instance.unit.Clone() }
func (s *Setup) Binding() UnitBinding      { return s.instance.binding.Clone() }
func (s *Setup) Snapshot() BindingSnapshot { return s.instance.snapshot.Clone() }
func (s *Setup) Configuration() []byte     { return bytes.Clone(s.instance.configuration) }
func (s *Setup) InstanceID() string        { return s.instance.id }
func (s *Setup) Generation() uint64        { return s.instance.generation }
func (s *Setup) Epoch() uint64             { return s.host.epoch }
func (s *Setup) ScopeGrant() ScopeGrant    { return s.instance.grant() }
func (s *Setup) Realm() RealmID            { return s.instance.realm }
func (s *Setup) Lifetime() *Lifetime       { return s.instance.lifetime }
func (s *Setup) Context() context.Context  { return s.work.Context() }

func (s *Setup) ValueContract(ref ContractRef) (ContractRef, error) {
	return s.host.ValueContract(ref)
}

func (s *Setup) ContractDescriptor(ref ContractRef) ([]byte, error) {
	return s.host.ContractDescriptor(ref)
}
func (s *Setup) Own(resource Resource, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	return s.work.Own(resource, dependencies...)
}
func (s *Setup) Cleanup(name string, closeResource func() error, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	return s.work.Cleanup(name, closeResource, dependencies...)
}

func (h *Host) OwnsSetup(s *Setup) bool { return h != nil && s != nil && s.host == h }

// ValidateCurrent checks the original instance and frozen binding together,
// before a driver exposes a previously prepared execution to its realm.
func (s *Setup) ValidateCurrent(ctx context.Context) error {
	if s == nil || s.host == nil || s.instance == nil {
		return ErrInvalid
	}
	return s.host.admission.Under(ctx, func() error {
		s.host.mu.Lock()
		defer s.host.mu.Unlock()
		return s.instance.validateCurrentLocked()
	})
}

func (s *Setup) OnActivate(activate func(context.Context) error) error {
	if s == nil || s.host == nil || activate == nil {
		return ErrInvalid
	}
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	if s.activation.sealed || s.instance.retiring {
		return ErrClosed
	}
	if s.activation.callback != nil {
		return ErrInvalid
	}
	s.activation.callback = activate
	return nil
}

func (s *Setup) sealPreparation() func(context.Context) error {
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	s.activation.sealed = true
	callback := s.activation.callback
	s.activation.callback = nil
	return callback
}

func (s *Setup) Imports(slot string) ([]BoundValue, error) {
	values, found := s.imports[slot]
	if !found {
		return nil, fmt.Errorf("%w: undeclared import slot %s", ErrInvalid, slot)
	}
	return slices.Clone(values), nil
}

// readImport returns the single value bound to the import the author expects.
// An optional import without a provider reports found=false.
func readImport[T any](setup *Setup, expected ImportDeclaration) (value T, found bool, err error) {
	if setup == nil || expected.Contract == (ContractRef{}) {
		return value, false, ErrInvalid
	}
	declared, exists := setup.instance.unit.importSlot(expected.Slot)
	if !exists {
		return value, false, fmt.Errorf("%w: undeclared import slot %q", ErrInvalid, expected.Slot)
	}
	if declared.Contract != expected.Contract || declared.Optional != expected.Optional {
		return value, false, ErrContractMismatch
	}
	values := setup.imports[expected.Slot]
	switch {
	case len(values) == 0 && expected.Optional:
		return value, false, nil
	case len(values) == 0:
		return value, false, ErrUnavailable
	case len(values) != 1 || values[0].Source.Contract != declared.Contract:
		return value, false, ErrContractMismatch
	case values[0].Local:
		return value, false, ErrUnavailable
	}
	value, ok := values[0].Value.(T)
	if !ok {
		return value, false, ErrContractMismatch
	}
	return value, true, nil
}

func Import[T any](setup *Setup, slot string, contract Contract[T]) (T, error) {
	value, _, err := readImport[T](setup, ImportDeclaration{Slot: slot, Contract: contract.Ref()})
	return value, err
}

func OptionalImport[T any](setup *Setup, slot string, contract Contract[T]) (T, bool, error) {
	return readImport[T](setup, ImportDeclaration{Slot: slot, Contract: contract.Ref(), Optional: true})
}

func Provide[T any](name string, contract Contract[T], value T) ExportValue {
	return ExportValue{Name: name, Contract: contract.Ref(), Value: value, Durable: true}
}

func Contribute[M, V any](name string, point Point[M, V], metadata M, value V) ExportValue {
	return ExportValue{Name: name, Contract: point.Ref(), Metadata: metadata, Value: value, Durable: true}
}

type Unit struct {
	Declaration UnitDeclaration
	Setup       SetupFunc
}

type Definition struct {
	Revision framevalue.Digest
	Units    []Unit
}

// Plugin is a reusable declaration. Installing it creates installation and
// instance ownership; constructing it never starts code.
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
			return nil, fmt.Errorf("%w: duplicate unit", ErrInvalid)
		}
		seen[unit.Declaration.ID] = true
		if unit.Declaration.Profile == NativeGo && unit.Setup == nil {
			return nil, fmt.Errorf("%w: native unit requires setup", ErrInvalid)
		}
		if unit.Declaration.Profile != NativeGo && unit.Setup != nil {
			return nil, fmt.Errorf("%w: non-native unit cannot supply native setup", ErrInvalid)
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
