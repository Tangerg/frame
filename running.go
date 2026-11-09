package frame

import (
	"context"
	"errors"
	"sync"
)

// RunningUnit is one start handle of an instance. Only the final handle
// requests retirement, so independent clients of the same grant cannot stop
// each other. Copies share the original claim, and a stopped claim retains only
// the instance's identity and completion.
type RunningUnit struct {
	claim *startClaim
}

type startClaim struct {
	id       string
	instance *unitInstance
	once     sync.Once
	retired  <-chan struct{}
}

func (h *Host) StartUnit(ctx context.Context, grant ScopeGrant, root UnitRef) (*RunningUnit, error) {
	plan, err := h.PlanUnit(ctx, "", root, nil)
	if err != nil {
		return nil, err
	}
	use, err := h.acquireActivated(ctx, UseConfig{Owner: h.lifetime, Scope: grant}, "", h.startBinding(plan, grant))
	if err != nil {
		return nil, err
	}
	handle, err := use.start(ctx, root)
	// A start handle controls retirement, but is not an active borrow. Keeping
	// that temporary Use alive would prevent a host-initiated disable from draining.
	_, closeErr := use.Close(context.WithoutCancel(ctx))
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, handle.Stop(ctx))
	}
	return handle, nil
}

func (h *Host) startBinding(plan *BindingPlan, grant ScopeGrant) activationBinding {
	bind := h.plannedBinding(plan)
	return func(admitted context.Context) (BindingSnapshot, []bindingRead, error) {
		binding, reads, err := bind(admitted)
		if err != nil {
			return BindingSnapshot{}, nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, root := range binding.UnitRoots {
			if instance := h.instances[instanceKey{source: root, realm: plan.realm}]; instance != nil {
				if err := instance.validateStartLocked(grant); err != nil {
					return BindingSnapshot{}, nil, err
				}
			}
		}
		return binding, reads, nil
	}
}

func (u *Use) start(ctx context.Context, root UnitRef) (*RunningUnit, error) {
	work, err := u.Enter(ctx)
	if err != nil {
		return nil, err
	}
	defer work.Finish()
	h := u.host
	h.mu.Lock()
	defer h.mu.Unlock()
	instance := u.instance(root)
	if instance == nil {
		return nil, ErrContractMismatch
	}
	return instance.claimStartLocked(u.scope)
}

func (i *unitInstance) validateStartLocked(grant ScopeGrant) error {
	if i.retiring {
		return ErrStaleBinding
	}
	if !i.startGrant.issued() || i.startGrant != grant {
		return ErrFenced
	}
	return nil
}

func (i *unitInstance) claimStartLocked(grant ScopeGrant) (*RunningUnit, error) {
	if err := i.validateStartLocked(grant); err != nil {
		return nil, err
	}
	i.starts++
	return &RunningUnit{claim: &startClaim{id: i.id, instance: i}}, nil
}

func (i *unitInstance) releaseStart() <-chan struct{} {
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

func (r *RunningUnit) ID() string {
	if r == nil || r.claim == nil {
		return ""
	}
	return r.claim.id
}

func (r *RunningUnit) Stop(ctx context.Context) error {
	if ctx == nil || r == nil || r.claim == nil {
		return ErrInvalid
	}
	claim := r.claim
	claim.once.Do(func() {
		claim.retired = claim.instance.releaseStart()
		claim.instance = nil
	})
	if claim.retired == nil {
		return nil
	}
	select {
	case <-claim.retired:
		return nil
	case <-ctx.Done():
		if isClosed(claim.retired) {
			return nil
		}
		return contextError(ctx)
	}
}
