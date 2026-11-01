package frame

import (
	"context"
	"errors"
	"sync"
)

func (r InstallationRecord) Unit(id UnitID) UnitRef {
	return UnitRef{InstallationID: r.ID, Release: r.Release, UnitID: id}
}

// RunningUnit holds the original instance. Only its final start handle requests
// retirement, so independent clients of the same grant cannot stop each other.
type RunningUnit struct {
	instance *Instance
	once     sync.Once
	last     bool
}

func (h *Host) StartUnit(ctx context.Context, grant ScopeGrant, root UnitRef) (*RunningUnit, error) {
	use, err := h.AcquireUnit(ctx, root, UseConfig{Owner: h.lifetime, Scope: grant})
	if err != nil {
		return nil, err
	}
	var instance *Instance
	err = BorrowInstance(ctx, use, root, func(_ context.Context, id string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		instance = h.allInstances[id]
		if instance == nil || instance.retiring {
			return ErrStaleBinding
		}
		instance.starts++
		return nil
	})
	// A start handle controls retirement, but is not an active borrow. Keeping
	// that temporary Use alive would prevent a host-initiated disable from draining.
	_, closeErr := use.Close(context.WithoutCancel(ctx))
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	handle := &RunningUnit{instance: instance}
	if closeErr != nil {
		return nil, errors.Join(closeErr, handle.Stop(ctx))
	}
	return handle, nil
}

func (r *RunningUnit) ID() string { return r.instance.id }

func (r *RunningUnit) Stop(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	i := r.instance
	h := i.host
	r.once.Do(func() {
		h.mu.Lock()
		i.starts--
		r.last = i.starts == 0
		if r.last {
			i.retiring = true
			h.withdrawInstanceLocked(i)
			if h.instances[i.key()] == i {
				delete(h.instances, i.key())
			}
		}
		h.mu.Unlock()
		if r.last {
			i.beginRetire()
		}
	})
	if r.last {
		return h.WaitInstanceRetired(ctx, i.id)
	}
	return nil
}
