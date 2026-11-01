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

type Setup struct {
	host     *Host
	instance *Instance
	work     *Work
	imports  map[string][]BoundValue
	staged   *stagedExports
}

func (s *Setup) Unit() UnitDeclaration     { return s.instance.unit.Clone() }
func (s *Setup) Binding() UnitBinding      { return s.instance.binding.Clone() }
func (s *Setup) Snapshot() BindingSnapshot { return s.instance.snapshot.Clone() }
func (s *Setup) Configuration() []byte     { return bytes.Clone(s.instance.configuration) }
func (s *Setup) InstanceID() string        { return s.instance.id }
func (s *Setup) Generation() uint64        { return s.instance.generation }
func (s *Setup) Epoch() uint64             { return s.host.epoch }
func (s *Setup) Scope() ScopeID            { return s.ScopeGrant().ID() }
func (s *Setup) ScopeGeneration() uint64   { return s.ScopeGrant().Generation() }
func (s *Setup) ScopeGrant() ScopeGrant {
	if s.instance.startGrant.owner != nil {
		return s.instance.startGrant
	}
	return s.instance.scope
}
func (s *Setup) Realm() RealmID           { return s.instance.realm }
func (s *Setup) Lifetime() *Lifetime      { return s.instance.lifetime }
func (s *Setup) Context() context.Context { return s.work.Context() }

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

func (s *Setup) OnActivate(activate func(context.Context) error) error {
	if activate == nil {
		return ErrInvalid
	}
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	if s.instance.activate != nil || s.instance.readyDone || s.instance.retiring {
		return ErrInvalid
	}
	s.instance.activate = activate
	return nil
}

func (s *Setup) Imports(slot string) ([]BoundValue, error) {
	values, found := s.imports[slot]
	if !found {
		return nil, fmt.Errorf("%w: undeclared import slot %s", ErrInvalid, slot)
	}
	return slices.Clone(values), nil
}

func readImport[T any](setup *Setup, slot string, contract Contract[T], optional bool) (T, bool, error) {
	var zero T
	if setup == nil || contract.value == nil {
		return zero, false, ErrInvalid
	}
	for _, declared := range setup.instance.binding.Imports {
		if declared.Slot != slot {
			continue
		}
		if declared.Contract != contract.Ref() || declared.Optional != optional {
			return zero, false, ErrContractMismatch
		}
		values := setup.imports[slot]
		if len(values) == 0 {
			if optional {
				return zero, false, nil
			}
			return zero, false, ErrUnavailable
		}
		if len(values) != 1 || values[0].Source.Contract != declared.Contract {
			return zero, false, ErrContractMismatch
		}
		if values[0].Local {
			return zero, false, ErrUnavailable
		}
		value, ok := values[0].Value.(T)
		if !ok {
			return zero, false, ErrContractMismatch
		}
		return value, true, nil
	}
	return zero, false, fmt.Errorf("%w: undeclared import slot %q", ErrInvalid, slot)
}

func Import[T any](setup *Setup, slot string, contract Contract[T]) (T, error) {
	value, _, err := readImport(setup, slot, contract, false)
	return value, err
}
func OptionalImport[T any](setup *Setup, slot string, contract Contract[T]) (T, bool, error) {
	return readImport(setup, slot, contract, true)
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
