package distribution

import (
	"context"
	"slices"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

// Resources exposes only a unit's declared immutable files. Reads return
// detached bytes; the provider's original lease fences access after retirement.
type Resources struct {
	lease Lease
	files []string
}

func (r *Resources) Files() []string { return slices.Clone(r.files) }

func (r *Resources) Read(ctx context.Context, path string) ([]byte, error) {
	if r == nil || !slices.Contains(r.files, path) {
		return nil, ErrInvalid
	}
	return r.lease.Read(ctx, path)
}

const contentResourceName = "static content"

type ResourceDriver struct{ store *storeAuthority }

func NewResourceDriver(store Store) (ResourceDriver, error) {
	if store.storeAuthority == nil {
		return ResourceDriver{}, frame.ErrInvalid
	}
	return ResourceDriver{store: store.storeAuthority}, nil
}

func (ResourceDriver) Profile() framehost.Profile { return framehost.StaticResource }

func (d ResourceDriver) Prepare(ctx context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
	if !setup.Valid() {
		return nil, ErrInvalid
	}
	lease, _, err := framehost.AcquireResource(setup, contentResourceName,
		func(context.Context) (Lease, error) {
			return d.store.Acquire(ctx, setup.Binding().Source.Release)
		},
		func(lease Lease) error { return lease.Close() },
	)
	if err != nil {
		return nil, err
	}
	declaration := setup.Unit()
	manifest := lease.Snapshot().Manifest
	declared, found := manifest.unit(declaration.ID)
	if !found || declared.Profile != framehost.StaticResource || len(declaration.Imports) != 0 || !slices.Equal(declared.Exports, declaration.Exports) {
		return nil, frame.ErrContractMismatch
	}
	resources := &Resources{lease: lease, files: slices.Clone(declared.Resources)}
	values := make([]framehost.ExportValue, 0, len(declaration.Exports))
	for _, export := range declaration.Exports {
		metadata, err := manifest.contributionMetadata(declaration.ID, export.Name)
		if err != nil {
			return nil, err
		}
		values = append(values, framehost.ExportValue{Name: export.Name, Contract: export.Contract, Metadata: metadata, Value: resources})
	}
	return values, nil
}

func (m Manifest) unit(id framehost.UnitID) (Unit, bool) {
	for _, unit := range m.Units {
		if unit.ID == id {
			return unit, true
		}
	}
	return Unit{}, false
}

func (m Manifest) contributionMetadata(unit framehost.UnitID, export string) (any, error) {
	for _, contribution := range m.Contributions {
		if contribution.Unit == unit && contribution.Export == export {
			return framevalue.ParseJSON(contribution.Metadata)
		}
	}
	return nil, nil
}
