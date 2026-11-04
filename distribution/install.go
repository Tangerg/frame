package distribution

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
)

// Archive integrity cannot prove runtime support; each execution adapter
// must admit its own entry shape before the Host installs it.
type ArchiveDriver interface {
	frame.Driver
	ValidateArchiveUnit(Unit) error
}

type Installation struct {
	ID            frame.InstallationID
	Enabled       bool
	Configuration []byte
	Native        func(frame.UnitID) (frame.SetupFunc, error)
}

// Slow profile validation cannot relinquish content before the Host accepts
// or rejects the original release.
func (l *Lease) Install(ctx context.Context, host *frame.Host, config Installation) (record frame.InstallationRecord, err error) {
	if l == nil || host == nil || host.Admission() != l.store.admission {
		return record, frame.ErrInvalid
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return record, frame.ErrClosed
	}
	digest := l.release.Digest
	l.mu.Unlock()
	pin, err := l.store.Acquire(ctx, digest)
	if err != nil {
		return record, err
	}
	defer func() { err = errors.Join(err, pin.Close()) }()
	release := pin.Snapshot()
	definition := frame.Definition{Revision: digest}
	for _, entry := range release.Manifest.Units {
		unit, e := entry.executableUnit(host, config.Native)
		if e != nil {
			return record, fmt.Errorf("distribution: unit %q: %w", entry.ID, e)
		}
		definition.Units = append(definition.Units, unit)
	}
	plugin, err := frame.Define(definition)
	if err != nil {
		return record, err
	}
	return host.Install(ctx, frame.InstallationConfig{ID: config.ID, Plugin: plugin, Configuration: config.Configuration, Enabled: config.Enabled})
}

func (entry Unit) executableUnit(host *frame.Host, native func(frame.UnitID) (frame.SetupFunc, error)) (frame.Unit, error) {
	unit := frame.Unit{Declaration: frame.UnitDeclaration{ID: entry.ID, Profile: entry.Profile, Exports: entry.Exports}}
	for _, slot := range entry.Imports {
		unit.Declaration.Imports = append(unit.Declaration.Imports, frame.ImportDeclaration{Slot: slot.Slot, Contract: slot.Contract, Optional: slot.Optional})
	}
	if entry.Profile == frame.NativeGo {
		if native == nil {
			return frame.Unit{}, frame.ErrUnavailable
		}
		fn, err := native(entry.ID)
		if err != nil {
			return frame.Unit{}, err
		}
		unit.Setup = fn
		return unit, nil
	}
	driver, ok := host.Driver(entry.Profile)
	if !ok {
		return frame.Unit{}, fmt.Errorf("%w: unsupported profile %s", frame.ErrUnavailable, entry.Profile)
	}
	if validator, ok := driver.(ArchiveDriver); ok {
		return unit, validator.ValidateArchiveUnit(entry.snapshot())
	}
	standard, err := entry.profileShape()
	if !standard {
		return frame.Unit{}, fmt.Errorf("%w: profile %s lacks an archive validator", frame.ErrUnavailable, entry.Profile)
	}
	return unit, err
}
