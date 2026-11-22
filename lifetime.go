package frame

import (
	"context"
	"sync"
	"weak"

	framevalue "github.com/Tangerg/frame/value"
)

type LifetimeLimits struct {
	MaxWork      int
	MaxResources int
}

// Copies share the owner; replacing a handle cannot redirect existing work.
type Lifetime struct{ *lifetimeRecord }

// Identity permits dependency inspection without granting control of the lifetime.
type LifetimeID struct{ owner *lifetimeRecord }

func (l *lifetimeRecord) Identity() LifetimeID { return LifetimeID{l} }

type lifetimeRecord struct {
	// Host-created lifetimes stay inside the admission domain that can inspect
	// their provider dependencies. Application lifetimes have no such affinity.
	authority       *LifetimeDomain
	mu              sync.Mutex
	ctx             context.Context
	cancelContext   context.CancelCauseFunc
	parentDone      <-chan struct{}
	stopParent      func() bool
	changed         chan struct{}
	work            map[*workInterval]struct{}
	holds           int
	resources       []*resourceEntry
	limits          LifetimeLimits
	closing         *closeOperation
	released        bool
	causes          weak.Pointer[CloseCauses]
	failures        []CloseFailure
	omittedFailures uint64
}

const (
	defaultLifetimeWork      = 4096
	defaultLifetimeResources = 4096
)

func NewLifetime(parent context.Context) Lifetime {
	lifetime, err := NewLifetimeWithLimits(parent, LifetimeLimits{})
	if err != nil {
		panic(err)
	}
	return lifetime
}

// Limits count resources until their original release is proved, including
// resources whose cleanup returned without a release conclusion.
func NewLifetimeWithLimits(parent context.Context, limits LifetimeLimits) (Lifetime, error) {
	if framevalue.IsNil(parent) {
		return Lifetime{}, ErrInvalid
	}
	var err error
	if limits.MaxWork, err = ResolveLimit("lifetime work", limits.MaxWork, defaultLifetimeWork); err != nil {
		return Lifetime{}, err
	}
	if limits.MaxResources, err = ResolveLimit("lifetime resources", limits.MaxResources, defaultLifetimeResources); err != nil {
		return Lifetime{}, err
	}
	ctx, cancel := context.WithCancelCause(parent)
	parentDone := parent.Done()
	// A custom parent's cancellation may precede its propagation callback.
	// Its original cause must reach the single context owner before local close.
	cancelContext := func(cause error) {
		if isClosed(parentDone) {
			cause = context.Cause(parent)
		}
		cancel(cause)
	}
	l := &lifetimeRecord{ctx: ctx, cancelContext: cancelContext, parentDone: parentDone, changed: make(chan struct{}), work: make(map[*workInterval]struct{}), limits: limits}
	stop := context.AfterFunc(parent, func() { l.BeginClose(context.Cause(parent)) })
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		stop()
	} else {
		l.stopParent = stop
		l.mu.Unlock()
	}
	return Lifetime{l}, nil
}

func (l Lifetime) Valid() bool { return l.lifetimeRecord != nil }

func (l *lifetimeRecord) Context() context.Context {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ctx
}

func (l *lifetimeRecord) IsClosing() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closingLocked()
}

func (l *lifetimeRecord) closingLocked() bool {
	// The parent's cancellation is already authoritative while its AfterFunc
	// notification is still queued. The notification only starts retirement.
	return l.closing != nil || l.released || l.ctx.Err() != nil || isClosed(l.parentDone)
}

func (l *lifetimeRecord) CloseOutcome() (CloseOutcome, bool) {
	if l == nil {
		return CloseOutcome{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing == nil {
		return CloseOutcome{}, false
	}
	return l.outcomeLocked(l.closing.id), true
}

func (l *lifetimeRecord) Cancel(cause error) {
	if l == nil {
		return
	}
	l.mu.Lock()
	cancel := l.cancelContext
	l.mu.Unlock()
	cancel(cause)
}

func (l *lifetimeRecord) signalLocked() { close(l.changed); l.changed = make(chan struct{}) }

func isClosed(signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	default:
		return false
	}
}
