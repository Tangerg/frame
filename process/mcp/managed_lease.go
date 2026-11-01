package mcp

import (
	"context"
	"errors"
	"sync"

	"github.com/Tangerg/frame"
)

// Reserve the lifetime slot before a broker can issue credentials. The runner's
// process exit cannot prove that previously admitted host callbacks have joined.
type managedLease struct {
	ready    chan struct{}
	grant    *ManagedCallback
	owner    *frame.ResourceHandle
	once     sync.Once
	revoked  chan struct{}
	released chan struct{}
	err      error
}

func reserveManagedLease(lifetime *frame.Lifetime) (*managedLease, error) {
	lease := &managedLease{ready: make(chan struct{}), revoked: make(chan struct{}), released: make(chan struct{})}
	var err error
	lease.owner, err = lifetime.Own(frame.Resource{Name: "managed MCP callback lease", Close: lease.close})
	return lease, err
}

func (l *managedLease) bind(grant *ManagedCallback) {
	l.grant = grant
	close(l.ready)
}

// A broker returning no grant on failure asserts that no permission escaped.
// A panic makes no such assertion and must leave the owner unconfirmed.
func (l *managedLease) issue(open func() (*ManagedCallback, error)) (grant *ManagedCallback, err error) {
	defer func() {
		if recover() != nil {
			grant = &ManagedCallback{}
			err = errors.New("mcp: callback broker panicked while issuing a grant")
		}
		l.bind(grant)
	}()
	return open()
}

func issuedGrant(grant ManagedCallback, err error) (*ManagedCallback, error) {
	if grant.Endpoint == "" && grant.Credential == "" && grant.Call == "" && grant.MaxResponseBytes == 0 && grant.Released == nil && grant.Revoke == nil {
		return nil, err
	}
	return &grant, err
}

func revokeManaged(grant *ManagedCallback) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("mcp: callback broker panicked while revoking a grant")
		}
	}()
	return grant.Revoke()
}

func (l *managedLease) revoke() {
	<-l.ready
	g := l.grant
	if g != nil {
		if g.Revoke != nil {
			l.err = revokeManaged(g)
		}
		if g.Released == nil {
			l.err = errors.Join(l.err, frame.ErrUnconfirmedRelease)
		}
	}
	close(l.revoked)
	if g != nil {
		if g.Released == nil {
			return
		}
		<-g.Released
	}
	_ = l.owner.ConfirmReleased(l.err)
	close(l.released)
}

func (l *managedLease) close(ctx context.Context) (frame.ReleaseState, error) {
	l.once.Do(func() { go l.revoke() })
	select {
	case <-l.revoked:
	case <-ctx.Done():
		return frame.ResourceUnconfirmed, context.Cause(ctx)
	}
	if l.err != nil {
		select {
		case <-l.released:
			return frame.ResourceReleased, l.err
		default:
			return frame.ResourceUnconfirmed, l.err
		}
	}
	select {
	case <-l.released:
		return frame.ResourceReleased, nil
	case <-ctx.Done():
		return frame.ResourceUnconfirmed, context.Cause(ctx)
	}
}

func (l *managedLease) finish(ctx context.Context) error {
	state, err := l.close(ctx)
	if state != frame.ResourceReleased {
		return errors.Join(frame.ErrUnconfirmedRelease, err)
	}
	return err
}
