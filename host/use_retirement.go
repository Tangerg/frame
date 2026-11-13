package host

import (
	"context"
	"errors"
	"sync"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type settlementState uint8

const (
	settlementPending settlementState = iota
	settlementFailed
	settlementRetrying
	settlementComplete
)

type settlementFailure struct {
	cause error
	retry chan struct{}
}

// Only the settlement advances release. Channels notify observers; an observed
// failure identity authorizes one retry without authorizing a later failure.
type settlement struct {
	mu      sync.Mutex
	state   settlementState
	failure *settlementFailure
	changed chan struct{}
	done    chan struct{}
}

func newSettlement() *settlement {
	return &settlement{changed: make(chan struct{}), done: make(chan struct{})}
}

func (s *settlement) observe() *settlementFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *settlement) fail(err error) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil || s.state != settlementPending && s.state != settlementRetrying {
		panic("host: invalid release settlement failure")
	}
	s.failure = &settlementFailure{cause: err, retry: make(chan struct{})}
	s.state = settlementFailed
	close(s.changed)
	s.changed = make(chan struct{})
	return s.failure.retry
}

func (s *settlement) complete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != settlementPending && s.state != settlementRetrying {
		panic("host: invalid release settlement completion")
	}
	s.state = settlementComplete
	s.failure = nil
	close(s.done)
	close(s.changed)
}

func (s *settlement) requestRetry() (*settlementFailure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == settlementFailed {
		s.state = settlementRetrying
		close(s.failure.retry)
	}
	return s.failure, s.state == settlementRetrying
}

func (s *settlement) await(ctx context.Context, observed *settlementFailure) error {
	var canceled error
	s.mu.Lock()
	for {
		switch s.state {
		case settlementComplete:
			s.mu.Unlock()
			return nil
		case settlementFailed, settlementRetrying:
			if s.state == settlementFailed || s.failure != observed {
				err := s.failure.cause
				s.mu.Unlock()
				return err
			}
		}
		if canceled != nil {
			s.mu.Unlock()
			return canceled
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			canceled = frame.ContextError(ctx)
		}
		s.mu.Lock()
	}
}

func (u *useBorrow) retireAfterClose(causes *frame.CloseCauses) {
	<-u.initialized
	operation := u.lifetime.BeginClose(frame.ErrClosed)
	// This join cannot be cancelled; cleanup diagnostics cannot invalidate its proof.
	outcome, _ := operation.WaitReleased(context.Background())
	proof, _ := operation.ReleaseProof()
	u.settleRelease(proof)
	u.retire(causes.Join(outcome.Err()))
	u.settlement.complete()
}

func (u *Use) BeginClose(cause error) frame.Close {
	if u == nil || u.borrow == nil {
		return frame.Close{}
	}
	return u.borrow.lifetime.BeginClose(cause)
}

// settleRelease durably releases a protected Use. A failed attempt fences new
// admission and only an explicit Use or Host close can request another attempt.
func (u *useBorrow) settleRelease(proof frame.ReleaseProof) {
	h := u.host
	for u.approvalStarted {
		err := frame.InvokeCallback("use settlement", func() error {
			return h.protection.SettleUse(context.Background(), u.id, proof)
		})
		if err == nil {
			return
		}
		h.admission.RequireRecovery(err)
		retry := u.settlement.fail(err)
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
	u.hostResource = nil
	u.ownerResource = nil
	u.owner = frame.Lifetime{}
	u.scope = ScopeGrant{}
	u.host = nil
	h.mu.Unlock()
	if ownerResource != nil {
		_ = ownerResource.ConfirmReleased(diagnostic)
	}
	if hostResource != nil {
		_ = hostResource.ConfirmReleased(diagnostic)
	}
}

func (u *Use) Close(ctx context.Context) (frame.CloseOutcome, error) {
	if framevalue.IsNil(ctx) || u == nil || u.borrow == nil {
		return frame.CloseOutcome{}, frame.ErrInvalid
	}
	borrow := u.borrow
	observed, _ := borrow.settlement.requestRetry()
	return borrow.close(ctx, observed)
}

func (u *useBorrow) close(ctx context.Context, observed *settlementFailure) (frame.CloseOutcome, error) {
	causes := u.lifetime.ObserveCloseCauses()
	outcome, err := u.lifetime.BeginClose(frame.ErrClosed).Wait(ctx)
	err = causes.Join(err)
	if !outcome.Released {
		return outcome, err
	}
	if settleErr := u.settlement.await(ctx, observed); settleErr != nil {
		outcome.Released = false
		outcome.Pending = append(outcome.Pending, useSettlementPrefix+u.id)
		return outcome, errors.Join(err, settleErr)
	}
	return outcome, err
}

// WaitRetired joins the original exit, including stable execution-protection
// settlement and removal of Host references. It never starts a second cleanup.
func (u *Use) WaitRetired(ctx context.Context) (frame.CloseOutcome, error) {
	if framevalue.IsNil(ctx) || u == nil || u.borrow == nil {
		return frame.CloseOutcome{}, frame.ErrInvalid
	}
	borrow := u.borrow
	if err := awaitRetired(ctx, borrow.settlement.done); err != nil {
		return frame.CloseOutcome{}, err
	}
	outcome, _ := borrow.lifetime.CloseOutcome()
	return outcome, outcome.Err()
}
