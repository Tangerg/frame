package frame

import (
	"context"
	"sync"
)

// settlement is the durable release of a protected Use. done closes exactly
// once, after the release has been settled and the Host forgot the Use.
type settlement struct {
	mu      sync.Mutex
	failure error
	reports uint64
	changed chan struct{}
	retry   chan struct{}
	done    chan struct{}
}

func newSettlement() *settlement {
	return &settlement{changed: make(chan struct{}), retry: make(chan struct{}, 1), done: make(chan struct{})}
}

func (s *settlement) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = err
	s.reports++
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *settlement) awaitRetry() { <-s.retry }

func (s *settlement) complete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = nil
	s.reports++
	close(s.done)
	close(s.changed)
}

// await requests one retry of a failed settlement and reports its next
// conclusion; an earlier failure is never reported as this attempt's result.
func (s *settlement) await(ctx context.Context) error {
	s.mu.Lock()
	reports := s.reports
	if s.failure != nil {
		select {
		case s.retry <- struct{}{}:
		default:
		}
	}
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
