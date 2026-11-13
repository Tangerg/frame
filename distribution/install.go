package distribution

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

// Archive integrity cannot prove runtime support; each execution adapter
// must admit its own entry shape before the Host installs it.
type ArchiveDriver interface {
	framehost.Driver
	ValidateArchiveUnit(Unit) error
}

type Installation struct {
	ID            framehost.InstallationID
	Enabled       bool
	Configuration []byte
	Native        func(framehost.UnitID) (framehost.SetupFunc, error)
}

// Slow profile validation cannot relinquish content before the Host accepts
// or rejects the original release.
func (l *Lease) Install(ctx context.Context, host framehost.Host, config Installation) (record framehost.InstallationRecord, err error) {
	if l == nil || l.claim == nil || !host.Valid() {
		return record, frame.ErrInvalid
	}
	claim := l.claim
	claim.mu.Lock()
	store := claim.store
	digest := claim.release.Digest
	claim.mu.Unlock()
	if store == nil {
		return record, frame.ErrClosed
	}
	if host.Admission() != store.admission {
		return record, frame.ErrInvalid
	}
	pin, err := store.Acquire(ctx, digest)
	if err != nil {
		return record, err
	}
	defer func() { err = errors.Join(err, pin.Close()) }()
	release := pin.Snapshot()
	definition := framehost.Definition{Revision: digest}
	for _, entry := range release.Manifest.Units {
		unit, e := entry.executableUnit(host, config.Native)
		if e != nil {
			return record, fmt.Errorf("distribution: unit %q: %w", entry.ID, e)
		}
		definition.Units = append(definition.Units, unit)
	}
	plugin, err := framehost.Define(definition)
	if err != nil {
		return record, err
	}
	return host.Install(ctx, framehost.InstallationConfig{ID: config.ID, Plugin: plugin, Configuration: config.Configuration, Enabled: config.Enabled})
}

func (entry Unit) executableUnit(host framehost.Host, native func(framehost.UnitID) (framehost.SetupFunc, error)) (framehost.Unit, error) {
	unit := framehost.Unit{Declaration: framehost.UnitDeclaration{ID: entry.ID, Profile: entry.Profile, Exports: entry.Exports}}
	for _, slot := range entry.Imports {
		unit.Declaration.Imports = append(unit.Declaration.Imports, framehost.ImportDeclaration{Slot: slot.Slot, Contract: slot.Contract, Optional: slot.Optional})
	}
	if entry.Profile == framehost.NativeGo {
		if native == nil {
			return framehost.Unit{}, framehost.ErrUnavailable
		}
		fn, err := native(entry.ID)
		if err != nil {
			return framehost.Unit{}, err
		}
		unit.Setup = fn
		return unit, nil
	}
	driver, ok := host.Driver(entry.Profile)
	if !ok {
		return framehost.Unit{}, fmt.Errorf("%w: unsupported profile %s", framehost.ErrUnavailable, entry.Profile)
	}
	if validator, ok := driver.(ArchiveDriver); ok {
		return unit, validator.ValidateArchiveUnit(entry.snapshot())
	}
	standard, err := entry.profileShape()
	if !standard {
		return framehost.Unit{}, fmt.Errorf("%w: profile %s lacks an archive validator", framehost.ErrUnavailable, entry.Profile)
	}
	return unit, err
}
