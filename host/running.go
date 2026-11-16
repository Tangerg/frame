package host

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// RunningUnit is one start handle of an instance. Only the final handle
// requests retirement, so independent clients of the same grant cannot stop
// each other. Copies share the original claim, and a stopped claim retains only
// the instance's identity and completion. Authoritative instance retirement
// ends every outstanding claim before joining the original resource exit.
type RunningUnit struct {
	claim *startClaim
}

type startClaim struct {
	id      string
	hold    instanceHold
	once    sync.Once
	retired <-chan struct{}
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
	var running RunningUnit
	handedOff := false
	defer func() {
		if !handedOff && running.Valid() {
			running.claim.stop()
		}
	}()
	var preparation *bindingPreparation
	err = h.admission.Admit(work.Context(), func(admitted context.Context) error {
		var err error
		h.mu.Lock()
		if current := h.instances[instanceKey{source: root}]; current != nil {
			err = current.validateStartLocked(grant)
		}
		h.mu.Unlock()
		if err != nil {
			return err
		}
		preparation, err = h.planPreparation(admitted, "", grant, h.plannedBinding(plan))
		if err != nil {
			return err
		}
		running, err = preparation.claimStart(root, grant)
		return err
	})
	if err != nil {
		return RunningUnit{}, err
	}
	prepared, err := preparation.prepare(work.Context())
	if err != nil {
		return RunningUnit{}, err
	}
	err = h.admission.Admit(work.Context(), func(context.Context) error {
		if err := h.admission.ValidateScope(grant); err != nil {
			return err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.validatePlanLocked(prepared)
	})
	if err != nil {
		return RunningUnit{}, err
	}
	handedOff = true
	return running, nil
}

func (i *unitInstance) validateStartLocked(grant ScopeGrant) error {
	if !i.isCurrentLocked() {
		return ErrStaleBinding
	}
	if !i.startGrant.Valid() || i.startGrant != grant {
		return ErrFenced
	}
	return nil
}

func (i *unitInstance) claimStartLocked(grant ScopeGrant) (RunningUnit, error) {
	if err := i.validateStartLocked(grant); err != nil {
		return RunningUnit{}, err
	}
	if err := i.host.admission.ValidateScope(grant); err != nil {
		return RunningUnit{}, err
	}
	hold, err := i.hold()
	if err != nil {
		return RunningUnit{}, err
	}
	claim := &startClaim{id: i.id, hold: hold}
	if i.startClaims == nil {
		i.startClaims = make(map[*startClaim]struct{})
	}
	i.startClaims[claim] = struct{}{}
	return RunningUnit{claim: claim}, nil
}

func (i *unitInstance) releaseStart(claim *startClaim) <-chan struct{} {
	h := i.host
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(i.startClaims, claim)
	claim.hold.work.Finish()
	if len(i.startClaims) != 0 && i.isCurrentLocked() {
		return nil
	}
	i.beginRetireLocked()
	return i.retired
}

// Withdrawal ends start admission. Stop outside Host.mu so an already running
// claim completion can acquire that same lock before joining its original exit.
func (i *unitInstance) endStartClaims() {
	h := i.host
	h.mu.Lock()
	claims := slices.Collect(maps.Keys(i.startClaims))
	h.mu.Unlock()
	for _, claim := range claims {
		claim.stop()
	}
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
	retired := r.claim.stop()
	if retired == nil {
		return nil
	}
	return awaitRetired(ctx, retired)
}

func (c *startClaim) stop() <-chan struct{} {
	c.once.Do(func() {
		c.retired = c.hold.instance.releaseStart(c)
		c.hold = instanceHold{}
	})
	return c.retired
}
