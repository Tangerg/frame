package process

import (
	"context"
	"errors"
	"sync"

	"github.com/Tangerg/frame/internal/cancellation"
)

var (
	errStreamClosed       = errors.New("process: stream is closed")
	errConcurrentDelivery = errors.New("process: concurrent chunk delivery")
	errPendingDelivery    = errors.New("process: stream ended with pending delivery")
	errConsumerPanic      = errors.New("process: stream consumer panicked")
)

// Each endpoint owns its accepted deliveries. A producer's final result cannot
// erase the consumer's rejection or overtake an acknowledgement still in flight.
type streamCall struct {
	ctx     context.Context
	abort   context.CancelCauseFunc
	mu      sync.Mutex
	pending chan struct{}
	closed  bool
	failure error
}

func newStreamCall(ctx context.Context) *streamCall {
	ctx, abort := context.WithCancelCause(ctx)
	return &streamCall{ctx: ctx, abort: abort}
}

func (s *streamCall) failLocked(cause error) error {
	if s.failure == nil {
		s.failure = errors.Join(ErrOutcomeUnknown, cause)
		s.abort(s.failure)
	}
	return s.failure
}

func (s *streamCall) deliver(wait context.Context, emit func(context.Context) error) (err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errStreamClosed
	}
	if s.pending != nil {
		err = s.failLocked(errConcurrentDelivery)
	}
	if s.failure != nil {
		err = s.failure
		s.mu.Unlock()
		return err
	}
	s.pending = make(chan struct{})
	s.mu.Unlock()
	defer func() {
		if recover() != nil {
			err = errConsumerPanic
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			err = s.failLocked(err)
		}
		close(s.pending)
		s.pending = nil
	}()
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	if wait == nil {
		return errors.New("process: chunk wait context is required")
	}
	if err := context.Cause(wait); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	stop := context.AfterFunc(wait, func() { cancel(context.Cause(wait)) })
	defer stop()
	defer cancel(nil)
	return errors.Join(emit(ctx), cancellation.Err(ctx))
}

func (s *streamCall) finish(err error) error {
	s.mu.Lock()
	s.closed = true
	pending := s.pending
	if pending != nil {
		s.failLocked(errPendingDelivery)
	}
	s.mu.Unlock()
	if pending != nil {
		<-pending
	}
	s.mu.Lock()
	err = errors.Join(err, s.failure)
	s.mu.Unlock()
	s.abort(errStreamClosed)
	return err
}
