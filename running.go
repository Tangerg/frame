package frame

import (
	"context"
	"errors"
	"sync"
)

// RunningUnit is one start handle of an instance. Only the final handle
// requests retirement, so independent clients of the same grant cannot stop
// each other. A stopped handle retains only the instance's completion.
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
	instance, err := use.start(ctx, root)
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

func (u *Use) start(ctx context.Context, root UnitRef) (*Instance, error) {
	work, err := u.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer work.Finish()
	h := u.host
	h.mu.Lock()
	defer h.mu.Unlock()
	instance := u.instance(root)
	switch {
	case instance == nil:
		return nil, ErrContractMismatch
	case instance.retiring:
		return nil, ErrStaleBinding
	}
	instance.starts++
	return instance, nil
}

func (i *Instance) releaseStart() <-chan struct{} {
	h := i.host
	h.mu.Lock()
	i.starts--
	final := i.starts == 0
	if final {
		h.retireInstanceLocked(i)
	}
	h.mu.Unlock()
	if !final {
		return nil
	}
	i.beginRetire()
	return i.closed
}

func (r *RunningUnit) ID() string { return r.id }

func (r *RunningUnit) Stop(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	r.once.Do(func() {
		r.retired = r.instance.releaseStart()
		r.instance = nil
	})
	if r.retired == nil {
		return nil
	}
	select {
	case <-r.retired:
		return nil
	case <-ctx.Done():
		if isClosed(r.retired) {
			return nil
		}
		return contextError(ctx)
	}
}
