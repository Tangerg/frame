package frame

import (
	"context"
	"errors"
	"sync"
)

func (r InstallationRecord) Unit(id UnitID) UnitRef {
	return UnitRef{InstallationID: r.ID, Release: r.Release, UnitID: id}
}

// Only the final start handle requests retirement, so independent clients of
// the same grant cannot stop each other. Stopped handles retain only its completion.
type RunningUnit struct {
	id       string
	instance *Instance
	once     sync.Once
	retired  <-chan struct{}
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
	handle := &RunningUnit{id: instance.id, instance: instance}
	if closeErr != nil {
		return nil, errors.Join(closeErr, handle.Stop(ctx))
	}
	return handle, nil
}

func (r *RunningUnit) ID() string { return r.id }

func (r *RunningUnit) Stop(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	r.once.Do(func() {
		i := r.instance
		h := i.host
		h.mu.Lock()
		i.starts--
		if i.starts == 0 {
			r.retired = i.closed
			i.retiring = true
			h.withdrawInstanceLocked(i)
			if h.instances[i.key()] == i {
				delete(h.instances, i.key())
			}
		}
		h.mu.Unlock()
		if r.retired != nil {
			i.beginRetire()
		}
		r.instance = nil
	})
	if r.retired == nil {
		return nil
	}
	select {
	case <-r.retired:
		return nil
	default:
	}
	select {
	case <-r.retired:
		return nil
	case <-ctx.Done():
		return contextError(ctx)
	}
}
