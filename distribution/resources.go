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
	if _, err := setup.Cleanup("static content", lease.Close); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	declaration := setup.Unit()
	var declared *Unit
	for _, unit := range lease.release.Manifest.Units {
		if unit.ID == declaration.ID {
			copy := unit
			declared = &copy
			break
		}
	}
	if declared == nil || declared.Profile != frame.StaticResource || len(declaration.Imports) != 0 || !slices.Equal(declared.Exports, declaration.Exports) {
		return nil, frame.ErrContractMismatch
	}
	value := &Resources{lease: lease, files: slices.Clone(declared.Resources)}
	values := make([]frame.ExportValue, 0, len(declaration.Exports))
	for _, export := range declaration.Exports {
		if export.Contract.Mode != frame.NativeMode {
			return nil, frame.ErrContractMismatch
		}
		var metadata any
		for _, contribution := range lease.release.Manifest.Contributions {
			if contribution.Unit == declaration.ID && contribution.Export == export.Name {
				if contribution.Point != export.Contract || metadata != nil {
					return nil, frame.ErrContractMismatch
				}
				admitted, err := framevalue.ParseJSON(contribution.Metadata)
				if err != nil {
					return nil, err
				}
				metadata = admitted
			}
		}
		values = append(values, frame.ExportValue{Name: export.Name, Contract: export.Contract, Metadata: metadata, Value: value, Durable: true})
	}
	return values, nil
}
