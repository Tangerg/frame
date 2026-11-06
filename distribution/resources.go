package distribution

import (
	"context"
	"errors"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// Resources exposes only a unit's declared immutable files. Reads return
// detached bytes; the provider's original lease fences access after retirement.
type Resources struct {
	lease *Lease
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

type ResourceDriver struct{ store *Store }

func NewResourceDriver(store *Store) (*ResourceDriver, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	return &ResourceDriver{store: store}, nil
}

func (*ResourceDriver) Profile() frame.Profile { return frame.StaticResource }

func (d *ResourceDriver) Prepare(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	if setup == nil {
		return nil, ErrInvalid
	}
	lease, err := d.store.Acquire(ctx, setup.Binding().Source.Release)
	if err != nil {
		return nil, err
	}
	if _, err := setup.Cleanup(contentResourceName, lease.Close); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	declaration := setup.Unit()
	manifest := lease.Snapshot().Manifest
	declared, found := manifest.unit(declaration.ID)
	if !found || declared.Profile != frame.StaticResource || len(declaration.Imports) != 0 || !slices.Equal(declared.Exports, declaration.Exports) {
		return nil, frame.ErrContractMismatch
	}
	resources := &Resources{lease: lease, files: slices.Clone(declared.Resources)}
	values := make([]frame.ExportValue, 0, len(declaration.Exports))
	for _, export := range declaration.Exports {
		metadata, err := manifest.contributionMetadata(declaration.ID, export.Name)
		if err != nil {
			return nil, err
		}
		values = append(values, frame.ExportValue{Name: export.Name, Contract: export.Contract, Metadata: metadata, Value: resources, Durable: true})
	}
	return values, nil
}

func (m Manifest) unit(id frame.UnitID) (Unit, bool) {
	for _, unit := range m.Units {
		if unit.ID == id {
			return unit, true
		}
	}
	return Unit{}, false
}

func (m Manifest) contributionMetadata(unit frame.UnitID, export string) (any, error) {
	for _, contribution := range m.Contributions {
		if contribution.Unit == unit && contribution.Export == export {
			return framevalue.ParseJSON(contribution.Metadata)
		}
	}
	return nil, nil
}
