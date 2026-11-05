package frame

import (
	"context"
	"sync"
)

// settlement is the durable release of a protected Use. done closes exactly
// once, after the release has been settled and the Host forgot the Use.
type settlement struct {
	mu             sync.Mutex
	failure        error
	reports        uint64
	changed        chan struct{}
	retryRequested chan struct{}
	done           chan struct{}
}

func newSettlement() *settlement {
	return &settlement{changed: make(chan struct{}), done: make(chan struct{})}
}

func (s *settlement) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = err
	s.reports++
	s.retryRequested = make(chan struct{})
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *settlement) awaitRetry() {
	s.mu.Lock()
	requested := s.retryRequested
	s.mu.Unlock()
	<-requested
}

func (s *settlement) complete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = nil
	s.reports++
	close(s.done)
	close(s.changed)
}

// A close can retry only a failure it observed on entry. A first failure during
// that close must be reported rather than silently starting another attempt.
func (s *settlement) requestRetry() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil && !isClosed(s.retryRequested) {
		close(s.retryRequested)
	}
	return s.reports
}

func (s *settlement) await(ctx context.Context, reports uint64) error {
	s.mu.Lock()
	for {
		if isClosed(s.done) {
			s.mu.Unlock()
			return nil
		}
		if s.reports > reports && s.failure != nil {
			failure := s.failure
			s.mu.Unlock()
			return failure
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return contextError(ctx)
		}
		s.mu.Lock()
	}
}
