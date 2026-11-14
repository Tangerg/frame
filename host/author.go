package host

import (
	"context"
	"fmt"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// Slots bind an author's language type to the same declaration inspected by the host.
type RequiredSlot[T any] struct {
	name     string
	contract Contract[T]
}

type OptionalSlot[T any] struct{ slot RequiredSlot[T] }

func Required[T any](name string, contract Contract[T]) RequiredSlot[T] {
	return RequiredSlot[T]{name: name, contract: contract}
}
func Optional[T any](name string, contract Contract[T]) OptionalSlot[T] {
	return OptionalSlot[T]{slot: Required(name, contract)}
}
func (s RequiredSlot[T]) importDeclaration() ImportDeclaration {
	return ImportDeclaration{Slot: s.name, Contract: s.contract.Ref()}
}
func (s OptionalSlot[T]) importDeclaration() ImportDeclaration {
	d := s.slot.importDeclaration()
	d.Optional = true
	return d
}
func (s RequiredSlot[T]) Get(setup Setup) (T, error) {
	return Import(setup, s.name, s.contract)
}
func (s OptionalSlot[T]) Get(setup Setup) (T, bool, error) {
	return OptionalImport(setup, s.slot.name, s.slot.contract)
}

type ImportSlot interface{ importDeclaration() ImportDeclaration }
type OutputSlot interface{ exportDeclaration() ExportDeclaration }

func Imports(slots ...ImportSlot) []ImportDeclaration {
	out := make([]ImportDeclaration, len(slots))
	for i, s := range slots {
		if !framevalue.IsNil(s) {
			out[i] = s.importDeclaration()
		}
	}
	return out
}
func Exports(slots ...OutputSlot) []ExportDeclaration {
	out := make([]ExportDeclaration, len(slots))
	for i, s := range slots {
		if !framevalue.IsNil(s) {
			out[i] = s.exportDeclaration()
		}
	}
	return out
}

type ServiceExport[T any] struct {
	name     string
	contract Contract[T]
}
type PointExport[M, V any] struct {
	name  string
	point Point[M, V]
}

func Export[T any](name string, contract Contract[T]) ServiceExport[T] {
	return ServiceExport[T]{name, contract}
}
func Offer[M, V any](name string, point Point[M, V]) PointExport[M, V] {
	return PointExport[M, V]{name, point}
}
func (s ServiceExport[T]) exportDeclaration() ExportDeclaration {
	return ExportDeclaration{Name: s.name, Contract: s.contract.Ref()}
}
func (s PointExport[M, V]) exportDeclaration() ExportDeclaration {
	return ExportDeclaration{Name: s.name, Contract: s.point.Ref()}
}
func (s ServiceExport[T]) Bind(setup Setup, value T) error {
	return setup.bind(Provide(s.name, s.contract, value))
}
func (s PointExport[M, V]) Bind(setup Setup, metadata M, value V) error {
	return setup.bind(Contribute(s.name, s.point, metadata, value))
}

type UnitSpec struct {
	ID      UnitID
	Imports []ImportDeclaration
	Exports []ExportDeclaration
	Prepare func(Setup) error
}

func DefineUnit(spec UnitSpec) (Unit, error) {
	d := UnitDeclaration{ID: spec.ID, Profile: NativeGo, Imports: spec.Imports, Exports: spec.Exports}
	if err := d.Validate(); err != nil {
		return Unit{}, err
	}
	prepare := spec.Prepare
	if prepare == nil {
		return Unit{}, fmt.Errorf("%w: native preparation required", frame.ErrInvalid)
	}
	return Unit{Declaration: d.Clone(), Setup: func(_ context.Context, s Setup) ([]ExportValue, error) {
		return nil, prepare(s)
	}}, nil
}

func (s Setup) bind(value ExportValue) error {
	if s.setupCall == nil {
		return frame.ErrInvalid
	}
	call := s.setupCall
	call.mu.Lock()
	defer call.mu.Unlock()
	if call.sealed {
		return frame.ErrClosed
	}
	d := ExportDeclaration{Name: value.Name, Contract: value.Contract}
	if !slices.Contains(call.instance.unit.Exports, d) {
		return frame.ErrContractMismatch
	}
	if slices.ContainsFunc(call.exports, func(e ExportValue) bool { return e.Name == value.Name }) {
		return fmt.Errorf("%w: duplicate export %q", frame.ErrInvalid, value.Name)
	}
	call.exports = append(call.exports, value)
	return nil
}

// AcquireResource reserves release capacity before opening the resource. The
// opener transfers ownership only on success; on failure it owns its rollback.
func AcquireResource[T any](setup Setup, name string, open func(context.Context) (T, error), closeResource func(T) error) (T, frame.ResourceHandle, error) {
	var value T
	if !setup.Valid() || open == nil || closeResource == nil {
		return value, frame.ResourceHandle{}, frame.ErrInvalid
	}
	work, err := setup.Lifetime().Enter(setup.Context())
	if err != nil {
		return value, frame.ResourceHandle{}, err
	}
	defer work.Finish()
	opened := false
	handle, err := work.Cleanup(name, func() error {
		if !opened {
			return nil
		}
		return closeResource(value)
	})
	if err != nil {
		return value, frame.ResourceHandle{}, err
	}
	value, err = open(work.Context())
	if err != nil {
		_ = handle.ConfirmReleased(nil)
		var zero T
		return zero, frame.ResourceHandle{}, err
	}
	opened = true
	return value, handle, nil
}
