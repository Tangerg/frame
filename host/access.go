package host

import (
	"context"
	"sync"

	"github.com/Tangerg/frame"
)

// Access borrows every selected member under one admitted work interval.
// Native implementations must not keep those values after the callback returns.
type Access struct{ interval *accessInterval }

// All handle copies lose access together before the original work is finished.
type accessInterval struct {
	mu      sync.Mutex
	use     *useBorrow
	ctx     context.Context
	work    frame.Work
	readers sync.WaitGroup
}

type Member[M, V any] struct {
	Source   ExportRef
	Metadata M
	Value    V
}

// Valid reports whether the handle names a borrow interval; the zero Access does not.
func (a Access) Valid() bool { return a.interval != nil }

func (a Access) Context() context.Context {
	if a.interval == nil {
		return nil
	}
	return a.interval.ctx
}

func (a *accessInterval) close() {
	a.mu.Lock()
	a.use = nil
	work := a.work
	a.work = frame.Work{}
	a.mu.Unlock()
	a.readers.Wait()
	work.Finish()
}

func withAccess(ctx context.Context, use Use, call func(Access) error) error {
	if use.borrow == nil || call == nil {
		return frame.ErrInvalid
	}
	borrow := use.borrow
	work, err := borrow.enter(ctx)
	if err != nil {
		return err
	}
	interval := &accessInterval{use: borrow, ctx: work.Context(), work: work}
	defer interval.close()
	return call(Access{interval: interval})
}

func (h *hostState) With(ctx context.Context, grant ScopeGrant, plan BindingPlan, call func(Access) error) error {
	if call == nil {
		return frame.ErrInvalid
	}
	return WithPlan(ctx, Host{h}, plan, grant, func(use Use) error { return withAccess(ctx, use, call) })
}

func (s Selection[M, V]) Members(access Access) ([]Member[M, V], error) {
	interval := access.interval
	if interval == nil {
		return nil, frame.ErrInvalid
	}
	interval.mu.Lock()
	use := interval.use
	if use == nil {
		interval.mu.Unlock()
		return nil, frame.ErrClosed
	}
	// Codecs may reenter this Access. The original work joins admitted reads
	// before releasing implementations; no state lock crosses user code.
	interval.readers.Add(1)
	interval.mu.Unlock()
	defer interval.readers.Done()
	ids, ok := use.selections[s.value.id]
	if !ok {
		return nil, frame.ErrContractMismatch
	}
	members := make([]Member[M, V], len(ids))
	for n, id := range ids {
		publisher := use.members[id]
		if publisher == nil {
			return nil, frame.ErrClosed
		}
		value, ok := publisher.value.(V)
		if !ok {
			return nil, frame.ErrContractMismatch
		}
		members[n].Source = publisher.source
		members[n].Value = value
		metadata, err := readMetadata[M](publisher.metadata)
		if err != nil {
			return nil, err
		}
		members[n].Metadata = metadata
	}
	interval.mu.Lock()
	defer interval.mu.Unlock()
	if interval.use == nil {
		return nil, frame.ErrClosed
	}
	return members, nil
}

func (s Selection[M, V]) One(access Access) (Member[M, V], error) {
	members, err := s.Members(access)
	if err != nil {
		return Member[M, V]{}, err
	}
	switch len(members) {
	case 0:
		return Member[M, V]{}, ErrUnavailable
	case 1:
		return members[0], nil
	default:
		return Member[M, V]{}, ErrAmbiguous
	}
}
