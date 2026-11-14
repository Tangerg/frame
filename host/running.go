package host

import (
	"context"
	"sync"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
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

func (h *hostState) StartUnit(ctx context.Context, grant ScopeGrant, root UnitRef) (RunningUnit, error) {
	work, err := h.lifetime.Enter(ctx)
	if err != nil {
		return RunningUnit{}, err
	}
	defer work.Finish()
	if err := h.admission.ValidateScope(grant); err != nil {
		return RunningUnit{}, err
	}
	plan, err := h.plan(work.Context(), "", nil, nil, []UnitRef{root})
	if err != nil {
		return RunningUnit{}, err
	}
	prepared, err := h.activate(work.Context(), "", grant, h.startBinding(plan, grant))
	if err != nil {
		return RunningUnit{}, err
	}
	var running RunningUnit
	err = h.admission.Admit(work.Context(), func(context.Context) error {
		if err := h.admission.ValidateScope(grant); err != nil {
			return err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := h.validatePlanLocked(prepared); err != nil {
			return err
		}
		instance := h.allInstances[prepared.unitMembers[0].instanceID]
		var err error
		running, err = instance.claimStartLocked(grant)
		return err
	})
	if err != nil {
		return RunningUnit{}, err
	}
	return running, nil
}

func (h *hostState) startBinding(plan *declaredPlan, grant ScopeGrant) activationBinding {
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

func (i *unitInstance) validateStartLocked(grant ScopeGrant) error {
	if !i.isCurrentLocked() {
		return ErrStaleBinding
	}
	if !i.startGrant.issued() || i.startGrant != grant {
		return ErrFenced
	}
	return nil
}

func (i *unitInstance) claimStartLocked(grant ScopeGrant) (RunningUnit, error) {
	if err := i.validateStartLocked(grant); err != nil {
		return RunningUnit{}, err
	}
	i.starts++
	return RunningUnit{claim: &startClaim{id: i.id, instance: i}}, nil
}

func (i *unitInstance) releaseStart() <-chan struct{} {
	h := i.host
	h.mu.Lock()
	i.starts--
	final := i.starts == 0
	if final {
		i.withdrawLocked()
	}
	h.mu.Unlock()
	if !final {
		return nil
	}
	i.beginRetire()
	return i.retirement.done
}

// Valid reports whether the handle names a start claim; the zero RunningUnit does not.
func (r RunningUnit) Valid() bool { return r.claim != nil }

func (r RunningUnit) ID() string {
	if r.claim == nil {
		return ""
	}
	return r.claim.id
}

func (r RunningUnit) Stop(ctx context.Context) error {
	if framevalue.IsNil(ctx) || r.claim == nil {
		return frame.ErrInvalid
	}
	claim := r.claim
	claim.once.Do(func() {
		claim.retired = claim.instance.releaseStart()
		claim.instance = nil
	})
	if claim.retired == nil {
		return nil
	}
	return awaitRetired(ctx, claim.retired)
}
