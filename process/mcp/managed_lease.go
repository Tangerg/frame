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
	owner    frame.ResourceHandle
	once     sync.Once
	revoked  chan struct{}
	released chan struct{}
	err      error
}

func reserveManagedLease(lifetime frame.Lifetime) (*managedLease, error) {
	lease := &managedLease{ready: make(chan struct{}), revoked: make(chan struct{}), released: make(chan struct{})}
	var err error
	lease.owner, err = lifetime.Own(frame.Resource{Name: "managed MCP callback lease", Close: lease.close})
	return lease, err
}

func (l *managedLease) bind(grant *ManagedCallback) *ManagedCallback {
	if grant != nil {
		copy := *grant
		grant = &copy
	}
	l.grant = grant
	close(l.ready)
	return grant
}

// A broker returning no grant on failure asserts that no permission escaped.
// An interrupted callback makes no such assertion and must leave the owner unconfirmed.
func (l *managedLease) issue(open func() (*ManagedCallback, error)) (grant *ManagedCallback, err error) {
	returned := false
	defer func() {
		if recovered := recover(); recovered != nil {
			grant = &ManagedCallback{}
			err = newCallbackPanic("mcp: callback broker panicked while issuing a grant", recovered)
		} else if !returned {
			grant = &ManagedCallback{}
			err = errors.New("mcp: callback broker did not return its grant")
		}
		grant = l.bind(grant)
	}()
	grant, err = open()
	returned = true
	return grant, err
}

func issuedGrant(grant ManagedCallback, err error) (*ManagedCallback, error) {
	if grant.Endpoint == "" && grant.Credential == "" && grant.Call == "" && grant.MaxResponseBytes == 0 && grant.Released == nil && grant.Revoke == nil {
		return nil, err
	}
	return &grant, err
}

func (l *managedLease) revoke() {
	<-l.ready
	g := l.grant
	returned := false
	// Revocation may interrupt its goroutine; only the original release proof
	// can settle the lease, so its observation must survive that interruption.
	defer func() {
		if recovered := recover(); recovered != nil {
			l.err = newCallbackPanic("mcp: callback broker panicked while revoking a grant", recovered)
		} else if !returned {
			l.err = errors.New("mcp: callback broker did not return from revocation")
		}
		if g != nil && g.Released == nil {
			l.err = errors.Join(l.err, frame.ErrUnconfirmedRelease)
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
	}()
	if g != nil && g.Revoke != nil {
		l.err = g.Revoke()
	}
	returned = true
}

func (l *managedLease) close(ctx context.Context) (frame.ReleaseState, error) {
	l.once.Do(func() { go l.revoke() })
	select {
	case <-l.revoked:
	case <-ctx.Done():
		return frame.ResourceUnconfirmed, frame.ContextError(ctx)
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
		return frame.ResourceUnconfirmed, frame.ContextError(ctx)
	}
}

func (l *managedLease) finish(ctx context.Context) error {
	state, err := l.close(ctx)
	if state != frame.ResourceReleased {
		return errors.Join(frame.ErrUnconfirmedRelease, err)
	}
	return err
}
