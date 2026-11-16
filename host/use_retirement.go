package host

import (
	"context"
	"sync"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type retirementState uint8

const (
	retirementPending retirementState = iota
	retirementFailed
	retirementRetrying
	retirementComplete
)

// Retry authority belongs to this failed attempt, not to its notification.
type releaseFailure struct {
	cause error
	retry chan struct{}
}

// Retirement retains original causes until both physical exit and settlement
// complete. Lifetime alone may already have discarded its weak observation.
type useRetirement struct {
	mu      sync.Mutex
	state   retirementState
	failure *releaseFailure
	changed chan struct{}
	causes  *frame.CloseCauses
	done    chan struct{}
}

func newUseRetirement(lifetime frame.Lifetime) *useRetirement {
	return &useRetirement{changed: make(chan struct{}), done: make(chan struct{}), causes: lifetime.ObserveCloseCauses()}
}

func (r *useRetirement) observeCauses() *frame.CloseCauses {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.causes == nil {
		return new(frame.CloseCauses)
	}
	return r.causes
}

func (r *useRetirement) observeFailure() *releaseFailure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func (r *useRetirement) fail(err error) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil || r.state != retirementPending && r.state != retirementRetrying {
		panic("host: invalid release settlement failure")
	}
	r.failure = &releaseFailure{cause: err, retry: make(chan struct{})}
	r.state = retirementFailed
	close(r.changed)
	r.changed = make(chan struct{})
	return r.failure.retry
}

func (r *useRetirement) complete() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != retirementPending && r.state != retirementRetrying {
		panic("host: invalid use retirement completion")
	}
	r.state = retirementComplete
	r.failure = nil
	r.causes = nil
	close(r.done)
	close(r.changed)
}

func (r *useRetirement) requestRetry() (*releaseFailure, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == retirementFailed {
		r.state = retirementRetrying
		close(r.failure.retry)
	}
	return r.failure, r.state == retirementRetrying
}

func (r *useRetirement) await(ctx context.Context, observed *releaseFailure) error {
	var canceled error
	r.mu.Lock()
	for {
		switch r.state {
		case retirementComplete:
			r.mu.Unlock()
			return nil
		case retirementFailed, retirementRetrying:
			if r.state == retirementFailed || r.failure != observed {
				err := r.failure.cause
				r.mu.Unlock()
				return err
			}
		}
		if canceled != nil {
			r.mu.Unlock()
			return canceled
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			canceled = frame.ContextError(ctx)
		}
		r.mu.Lock()
	}
}

func (u *useBorrow) retireAfterClose() {
	<-u.initialized
	causes := u.retirement.observeCauses()
	operation := u.lifetime.BeginClose(frame.ErrClosed)
	// This join cannot be cancelled; cleanup diagnostics cannot invalidate its proof.
	outcome, _ := operation.WaitReleased(context.Background())
	proof, _ := operation.ReleaseProof()
	u.settleRelease(proof)
	u.retire(causes.Join(outcome.Err()))
	u.retirement.complete()
}

// settleRelease durably releases a protected Use. A failed attempt fences new
// admission and only an explicit Use or Host close can request another attempt.
func (u *useBorrow) settleRelease(proof frame.ReleaseProof) {
	h := u.host
	if u.approvalCommit == NotCommitted {
		return
	}
	for {
		err := frame.InvokeCallback("use settlement", func() error {
			return h.durable.SettleUse(context.Background(), u.id, proof)
		})
		if err == nil {
			return
		}
		h.admission.RequireRecovery(err)
		retry := u.retirement.fail(err)
		<-retry
	}
}

// Actual hold release and removal from Host commit together. Diagnostic
// forwarding can reenter admission and must observe the released edges.
func (u *useBorrow) retire(diagnostic error) {
	for _, stop := range u.stops {
		stop()
	}
	h := u.host
	h.mu.Lock()
	for _, hold := range u.instanceHolds {
		hold.work.Finish()
	}
	ownerResource, hostResource := u.ownerResource, u.hostResource
	if h.uses[u.id] == u {
		delete(h.uses, u.id)
	}
	u.members = nil
	u.selections = nil
	u.instanceHolds = nil
	u.stops = nil
	u.hostResource = frame.ResourceHandle{}
	u.ownerResource = frame.ResourceHandle{}
	u.owner = frame.Lifetime{}
	u.scope = ScopeGrant{}
	u.host = nil
	h.mu.Unlock()
	if ownerResource.Valid() {
		_ = ownerResource.ConfirmReleased(diagnostic)
	}
	if hostResource.Valid() {
		_ = hostResource.ConfirmReleased(diagnostic)
	}
}

func (u Use) Close(ctx context.Context) (frame.CloseOutcome, error) {
	if framevalue.IsNil(ctx) || u.borrow == nil {
		return frame.CloseOutcome{}, frame.ErrInvalid
	}
	borrow := u.borrow
	observed, _ := borrow.retirement.requestRetry()
	return borrow.close(ctx, observed)
}

func (u *useBorrow) close(ctx context.Context, observed *releaseFailure) (frame.CloseOutcome, error) {
	causes := u.retirement.observeCauses()
	outcome, err := u.lifetime.BeginClose(frame.ErrClosed).Wait(ctx)
	if !outcome.Released {
		return outcome, causes.Join(err)
	}
	if settleErr := u.retirement.await(ctx, observed); settleErr != nil {
		outcome.Released = false
		outcome.Pending = append(outcome.Pending, useRetirementPrefix+u.id)
		return outcome, causes.Join(err, settleErr)
	}
	return outcome, causes.Join(err)
}

// WaitRetired joins the original exit, including stable execution-protection
// settlement and removal of Host references. It never starts a second cleanup.
func (u Use) WaitRetired(ctx context.Context) (frame.CloseOutcome, error) {
	if framevalue.IsNil(ctx) || u.borrow == nil {
		return frame.CloseOutcome{}, frame.ErrInvalid
	}
	borrow := u.borrow
	if err := awaitRetired(ctx, borrow.retirement.done); err != nil {
		return frame.CloseOutcome{}, err
	}
	outcome, _ := borrow.lifetime.CloseOutcome()
	return outcome, outcome.Err()
}
