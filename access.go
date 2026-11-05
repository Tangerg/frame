package frame

import (
	"context"
	"sync"
)

// Access borrows every selected member under one admitted work interval.
// Native implementations must not keep those values after the callback returns.
type Access struct{ interval *accessInterval }

// All handle copies lose access together before the original work is finished.
type accessInterval struct {
	mu  sync.Mutex
	use *Use
	ctx context.Context
}

type Member[M, V any] struct {
	Source   ExportRef
	Metadata M
	Value    V
}

func (a *Access) Context() context.Context {
	if a.interval == nil {
		return nil
	}
	return a.interval.ctx
}

func (a *accessInterval) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.use = nil
}

func withAccess(ctx context.Context, use *Use, call func(*Access) error) error {
	if use == nil || call == nil {
		return ErrInvalid
	}
	work, err := use.enter(ctx)
	if err != nil {
		return err
	}
	defer work.Finish()
	interval := &accessInterval{use: use, ctx: work.Context()}
	defer interval.close()
	return call(&Access{interval: interval})
}

func (h *Host) With(ctx context.Context, grant ScopeGrant, plan *BindingPlan, call func(*Access) error) error {
	if call == nil {
		return ErrInvalid
	}
	return WithPlan(ctx, h, plan, grant, func(use *Use) error { return withAccess(ctx, use, call) })
}

func (s Selection[M, V]) Members(access *Access) ([]Member[M, V], error) {
	if access == nil {
		return nil, ErrInvalid
	}
	interval := access.interval
	if interval == nil {
		return nil, ErrClosed
	}
	interval.mu.Lock()
	defer interval.mu.Unlock()
	use := interval.use
	if use == nil {
		return nil, ErrClosed
	}
	ids, ok := use.selections[s.value.id]
	if !ok {
		return nil, ErrContractMismatch
	}
	members := make([]Member[M, V], len(ids))
	for n, id := range ids {
		publisher := use.members[id]
		if publisher == nil {
			return nil, ErrClosed
		}
		value, ok := publisher.value.(V)
		if !ok {
			return nil, ErrContractMismatch
		}
		members[n].Source = publisher.source
		members[n].Value = value
		if publisher.metadata != nil {
			metadata, err := snapshotMetadata(publisher.metadata)
			if err != nil {
				return nil, err
			}
			v, ok := metadata.(M)
			if !ok {
				return nil, ErrContractMismatch
			}
			members[n].Metadata = v
		}
	}
	return members, nil
}

func (s Selection[M, V]) One(access *Access) (Member[M, V], error) {
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
