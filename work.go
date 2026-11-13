package frame

import (
	"context"
	"fmt"

	framevalue "github.com/Tangerg/frame/value"
)

// workKind separates a Lifetime's own work, which closing cancels, from a
// consumer's hold, which retirement drains before it closes anything.
type workKind uint8

const (
	ownWork workKind = iota
	consumerHold
)

// Work handles share the original interval, so copying a handle cannot reopen
// finished work or release another interval's capacity.
type Work struct {
	interval *workInterval
}

type workInterval struct {
	lifetime *lifetimeRecord
	ctx      context.Context
	cancel   context.CancelCauseFunc
	kind     workKind
	finished bool
}

func (l *lifetimeRecord) Enter(ctx context.Context) (*Work, error) {
	return l.enter(ctx, ownWork)
}

// hold registers a consumer that retirement drains instead of cancelling. Its
// context ends only when the Lifetime itself begins closing.
func (l *lifetimeRecord) Hold() (*Work, error) {
	return l.enter(context.Background(), consumerHold)
}

func (l *lifetimeRecord) enter(ctx context.Context, kind workKind) (*Work, error) {
	if l == nil {
		return nil, ErrInvalid
	}
	if err := ContextError(ctx); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closingLocked() {
		return nil, ErrClosed
	}
	if len(l.work) >= l.limits.MaxWork {
		return nil, fmt.Errorf("%w: lifetime work", ErrCapacity)
	}
	child, cancel := context.WithCancelCause(ctx)
	work := &workInterval{lifetime: l, ctx: child, cancel: cancel, kind: kind}
	l.work[work] = struct{}{}
	if kind == consumerHold {
		l.holds++
	}
	return &Work{interval: work}, nil
}

func (l *lifetimeRecord) AwaitUnheld(ctx context.Context) error {
	if l == nil || framevalue.IsNil(ctx) {
		return ErrInvalid
	}
	for {
		l.mu.Lock()
		holds, changed := l.holds, l.changed
		l.mu.Unlock()
		if holds == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ContextError(ctx)
		}
	}
}

func (w *Work) Context() context.Context { return w.interval.ctx }

func (w *Work) Finish() {
	if w == nil || w.interval == nil {
		return
	}
	interval := w.interval
	l := interval.lifetime
	l.mu.Lock()
	if interval.finished {
		l.mu.Unlock()
		return
	}
	interval.finished = true
	delete(l.work, interval)
	if interval.kind == consumerHold {
		l.holds--
	}
	l.signalLocked()
	l.mu.Unlock()
	interval.cancel(ErrClosed)
}

// Own accepts a late resource only while the work which acquired it is still
// registered. Retirement waits for that work before walking its resources.
func (w *Work) Own(resource Resource, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	if w == nil || w.interval == nil {
		return nil, ErrInvalid
	}
	l := w.interval.lifetime
	l.mu.Lock()
	if w.interval.finished {
		l.mu.Unlock()
		return nil, ErrClosed
	}
	handle, err := l.ownLocked(resource, dependencies)
	closing := l.closing != nil
	cause := context.Cause(l.ctx)
	l.mu.Unlock()
	if err == nil && closing && resource.Cancel != nil {
		if cancelErr := invokeCancel(resource.Cancel, cause); cancelErr != nil {
			detail := closeFailureDetail(cancelErr)
			l.mu.Lock()
			l.recordFailureLocked(handle.entry.resource.Name, detail, cancelErr)
			l.mu.Unlock()
		}
	}
	return handle, err
}

func (w *Work) Cleanup(name string, closeResource func() error, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	return w.Own(cleanupResource(name, closeResource), dependencies...)
}
func (l *lifetimeRecord) Go(ctx context.Context, run func(context.Context)) error {
	if run == nil {
		return ErrInvalid
	}
	work, err := l.Enter(ctx)
	if err != nil {
		return err
	}
	go func() { defer work.Finish(); run(work.Context()) }()
	return nil
}
