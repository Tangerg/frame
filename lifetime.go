package frame

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

type ReleaseState string

const (
	ResourceReleased    ReleaseState = "released"
	ResourceUnconfirmed ReleaseState = "unconfirmed"
)

var ErrUnconfirmedRelease = errors.New("frame: resource release unconfirmed")

// Resource describes actual owned work. Cancel must only request cancellation;
// Close supplies the release conclusion. An error alone never proves release.
type Resource struct {
	Name   string
	Cancel func(error)
	Close  func(context.Context) (ReleaseState, error)
}

type CloseFailure struct {
	Resource string `json:"resource"`
	Detail   string `json:"detail"`
}

type CloseOutcome struct {
	ID              uint64         `json:"id"`
	Released        bool           `json:"released"`
	Pending         []string       `json:"pending"`
	Failures        []CloseFailure `json:"failures"`
	OmittedFailures uint64         `json:"omittedFailures"`
}

type resourceEntry struct {
	id           uint64
	resource     Resource
	dependencies []*resourceEntry
	attempted    bool
	running      bool
	released     bool
}

// ResourceHandle reports evidence to the original resource owner. It cannot
// restart cleanup or transfer release ownership to a different lifetime.
type ResourceHandle struct {
	lifetime *Lifetime
	entry    *resourceEntry
}

type LifetimeLimits struct {
	MaxWork      int
	MaxResources int
}

type Lifetime struct {
	mu              sync.Mutex
	ctx             context.Context
	cancelContext   context.CancelCauseFunc
	stopParent      func() bool
	changed         chan struct{}
	work            map[*Work]struct{}
	resources       []*resourceEntry
	limits          LifetimeLimits
	nextResource    uint64
	closing         *Close
	released        bool
	failures        []CloseFailure
	omittedFailures uint64
}

type Work struct {
	lifetime *Lifetime
	ctx      context.Context
	cancel   context.CancelCauseFunc
	finished bool
}

// Close is one owned retirement, independent of all callers waiting for it.
type Close struct {
	lifetime    *Lifetime
	id          uint64
	settled     chan struct{}
	settledOnce sync.Once
}

var closeSequence atomic.Uint64

func NewLifetime(parent context.Context) *Lifetime {
	lifetime, err := NewLifetimeWithLimits(parent, LifetimeLimits{})
	if err != nil {
		panic(err)
	}
	return lifetime
}

// NewLifetimeWithLimits bounds admitted work and resources whose original
// release is not yet proved. Zero fields use the default limit of 4096.
func NewLifetimeWithLimits(parent context.Context, limits LifetimeLimits) (*Lifetime, error) {
	if parent == nil {
		return nil, ErrInvalid
	}
	if limits.MaxWork == 0 {
		limits.MaxWork = 4096
	}
	if limits.MaxResources == 0 {
		limits.MaxResources = 4096
	}
	if limits.MaxWork < 1 || limits.MaxResources < 1 {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	l := &Lifetime{ctx: ctx, cancelContext: cancel, changed: make(chan struct{}), work: make(map[*Work]struct{}), limits: limits}
	stop := context.AfterFunc(parent, func() { l.BeginClose(context.Cause(parent)) })
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		stop()
	} else {
		l.stopParent = stop
		l.mu.Unlock()
	}
	return l, nil
}

func (l *Lifetime) Context() context.Context { l.mu.Lock(); defer l.mu.Unlock(); return l.ctx }

func (l *Lifetime) cancel(cause error) {
	l.mu.Lock()
	cancel := l.cancelContext
	l.mu.Unlock()
	cancel(cause)
}

func (l *Lifetime) signalLocked() { close(l.changed); l.changed = make(chan struct{}) }

func (l *Lifetime) Enter(ctx context.Context) (*Work, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing != nil || l.released || l.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if len(l.work) >= l.limits.MaxWork {
		return nil, fmt.Errorf("%w: lifetime work", ErrCapacity)
	}
	child, cancel := context.WithCancelCause(ctx)
	work := &Work{lifetime: l, ctx: child, cancel: cancel}
	l.work[work] = struct{}{}
	return work, nil
}

func (w *Work) Context() context.Context { return w.ctx }

func (w *Work) Finish() {
	if w == nil || w.lifetime == nil {
		return
	}
	l := w.lifetime
	l.mu.Lock()
	if w.finished {
		l.mu.Unlock()
		return
	}
	w.finished = true
	delete(l.work, w)
	l.signalLocked()
	l.mu.Unlock()
	w.cancel(ErrClosed)
}

// Own accepts a late resource only while the work which acquired it is still
// registered. Retirement waits for that work before walking its resources.
func (w *Work) Own(resource Resource, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	if w == nil || w.lifetime == nil {
		return nil, ErrInvalid
	}
	l := w.lifetime
	l.mu.Lock()
	if w.finished {
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
			l.recordFailureLocked(resource.Name, detail)
			l.mu.Unlock()
		}
	}
	return handle, err
}

func (l *Lifetime) Own(resource Resource, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing != nil || l.released {
		return nil, ErrClosed
	}
	return l.ownLocked(resource, dependencies)
}

func (l *Lifetime) ownLocked(resource Resource, dependencies []*ResourceHandle) (*ResourceHandle, error) {
	if resource.Name == "" || resource.Close == nil {
		return nil, ErrInvalid
	}
	if len(l.resources) >= l.limits.MaxResources {
		return nil, fmt.Errorf("%w: lifetime resources", ErrCapacity)
	}
	entry := &resourceEntry{resource: resource}
	seen := make(map[*resourceEntry]bool)
	for _, dependency := range dependencies {
		if dependency == nil || dependency.lifetime != l || dependency.entry.released {
			return nil, fmt.Errorf("%w: resource dependency", ErrInvalid)
		}
		if seen[dependency.entry] {
			continue
		}
		seen[dependency.entry] = true
		entry.dependencies = append(entry.dependencies, dependency.entry)
	}
	l.nextResource++
	entry.id = l.nextResource
	l.resources = append(l.resources, entry)
	l.signalLocked()
	return &ResourceHandle{lifetime: l, entry: entry}, nil
}

func (l *Lifetime) Cleanup(name string, closeResource func() error, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	if closeResource == nil {
		return nil, ErrInvalid
	}
	return l.Own(Resource{Name: name, Close: func(context.Context) (ReleaseState, error) {
		err := closeResource()
		if err != nil {
			return ResourceUnconfirmed, err
		}
		return ResourceReleased, nil
	}}, dependencies...)
}

func (w *Work) Cleanup(name string, closeResource func() error, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	if closeResource == nil {
		return nil, ErrInvalid
	}
	return w.Own(Resource{Name: name, Close: func(context.Context) (ReleaseState, error) {
		err := closeResource()
		if err != nil {
			return ResourceUnconfirmed, err
		}
		return ResourceReleased, nil
	}}, dependencies...)
}

func (l *Lifetime) Go(ctx context.Context, run func(context.Context)) error {
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

func (l *Lifetime) BeginClose(cause error) *Close {
	l.mu.Lock()
	if l.closing != nil {
		result := l.closing
		l.mu.Unlock()
		return result
	}
	if cause == nil {
		cause = ErrClosed
	}
	operation := &Close{lifetime: l, id: closeSequence.Add(1), settled: make(chan struct{})}
	l.closing = operation
	work := make([]*Work, 0, len(l.work))
	for item := range l.work {
		work = append(work, item)
	}
	resources := slices.Clone(l.resources)
	cancellations := make([]func(error), 0, len(resources))
	for _, resource := range resources {
		if resource.resource.Cancel != nil {
			cancellations = append(cancellations, resource.resource.Cancel)
		}
	}
	l.signalLocked()
	l.mu.Unlock()
	l.cancel(cause)
	// Seal synchronously, then deliver every cancellation before waiting for any
	// work or resource. Cleanup callbacks always execute outside state locks.
	for _, item := range work {
		item.cancel(cause)
	}
	go l.retire(operation, cancellations, cause)
	return operation
}

func (l *Lifetime) retire(operation *Close, cancellations []func(error), cause error) {
	for _, cancel := range cancellations {
		if err := invokeCancel(cancel, cause); err != nil {
			detail := closeFailureDetail(err)
			l.mu.Lock()
			l.recordFailureLocked("cancellation", detail)
			l.mu.Unlock()
		}
	}
	for {
		l.mu.Lock()
		if len(l.work) == 0 {
			l.mu.Unlock()
			break
		}
		changed := l.changed
		l.mu.Unlock()
		<-changed
	}
	for {
		l.mu.Lock()
		var ready []*resourceEntry
		remaining, running := 0, 0
		for _, entry := range l.resources {
			if entry.released && !entry.running {
				continue
			}
			remaining++
			if entry.running {
				running++
				continue
			}
			if entry.attempted {
				continue
			}
			blocked := false
			for _, consumer := range l.resources {
				if consumer.released && !consumer.running {
					continue
				}
				if slices.Contains(consumer.dependencies, entry) {
					blocked = true
					break
				}
			}
			if !blocked {
				entry.attempted = true
				entry.running = true
				ready = append(ready, entry)
			}
		}
		if remaining == 0 {
			l.released = true
			for _, entry := range l.resources {
				entry.resource = Resource{Name: entry.resource.Name}
				entry.dependencies = nil
			}
			l.resources = nil
			l.work = nil
			stop := l.stopParent
			l.stopParent = nil
			terminal, cancel := context.WithCancelCause(context.Background())
			cancel(ErrClosed)
			l.ctx = terminal
			l.cancelContext = cancel
			l.signalLocked()
			l.mu.Unlock()
			if stop != nil {
				stop()
			}
			operation.settledOnce.Do(func() { close(operation.settled) })
			return
		}
		changed := l.changed
		l.mu.Unlock()
		if len(ready) == 0 && running == 0 {
			operation.settledOnce.Do(func() { close(operation.settled) })
		}
		for _, entry := range ready {
			l.mu.Lock()
			closeResource := entry.resource.Close
			l.mu.Unlock()
			go l.closeResource(entry, closeResource)
		}
		<-changed
	}
}

func invokeCancel(cancel func(error), cause error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("resource cancellation panicked: %v", value)
		}
	}()
	cancel(cause)
	return nil
}

func invokeResourceClose(closeResource func(context.Context) (ReleaseState, error), ctx context.Context) (state ReleaseState, err error) {
	state = ResourceUnconfirmed
	defer func() {
		if value := recover(); value != nil {
			state = ResourceUnconfirmed
			err = fmt.Errorf("resource close panicked: %v", value)
		}
	}()
	state, err = closeResource(ctx)
	if state != ResourceReleased && state != ResourceUnconfirmed {
		return ResourceUnconfirmed, errors.Join(err, fmt.Errorf("%w: resource close state", ErrInvalid))
	}
	return state, err
}

func (l *Lifetime) closeResource(entry *resourceEntry, closeResource func(context.Context) (ReleaseState, error)) {
	state, err := invokeResourceClose(closeResource, context.WithoutCancel(l.Context()))
	detail := closeFailureDetail(err)
	l.mu.Lock()
	entry.running = false
	if state == ResourceReleased {
		entry.released = true
	}
	if err != nil {
		l.recordFailureLocked(entry.resource.Name, detail)
	}
	if entry.released {
		entry.resource.Cancel = nil
		entry.resource.Close = nil
		entry.dependencies = nil
		l.resources = slices.DeleteFunc(l.resources, func(candidate *resourceEntry) bool { return candidate == entry })
	}
	l.signalLocked()
	l.mu.Unlock()
}

// Error formatting may call back into its resource owner. Keep it outside the
// state lock, and retain a bounded valid JSON string as the diagnostic projection.
func closeFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	const maxDetailBytes = 2048
	detail := strings.ToValidUTF8(fmt.Sprint(err), "\uFFFD")
	if len(detail) > maxDetailBytes {
		detail = detail[:maxDetailBytes]
		for !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}
	return detail
}

func (l *Lifetime) recordFailureLocked(name, detail string) {
	const maxFailures = 32
	if len(l.failures) == maxFailures {
		l.omittedFailures++
		return
	}
	l.failures = append(l.failures, CloseFailure{Resource: name, Detail: detail})
}

func (h *ResourceHandle) ConfirmReleased(diagnostic error) error {
	if h == nil || h.lifetime == nil || h.entry == nil {
		return ErrInvalid
	}
	detail := closeFailureDetail(diagnostic)
	l := h.lifetime
	l.mu.Lock()
	defer l.mu.Unlock()
	if h.entry.released {
		return nil
	}
	h.entry.released = true
	if diagnostic != nil {
		l.recordFailureLocked(h.entry.resource.Name, detail)
	}
	if !h.entry.running {
		h.entry.resource.Cancel = nil
		h.entry.resource.Close = nil
		h.entry.dependencies = nil
		l.resources = slices.DeleteFunc(l.resources, func(entry *resourceEntry) bool { return entry == h.entry })
	}
	l.signalLocked()
	return nil
}

func (l *Lifetime) outcomeLocked(id uint64) CloseOutcome {
	outcome := CloseOutcome{ID: id, Released: l.released, Failures: slices.Clone(l.failures), OmittedFailures: l.omittedFailures, Pending: []string{}}
	for _, entry := range l.resources {
		if !entry.released {
			outcome.Pending = append(outcome.Pending, entry.resource.Name)
		}
	}
	if len(l.work) > 0 {
		outcome.Pending = append(outcome.Pending, fmt.Sprintf("%d active work items", len(l.work)))
	}
	return outcome
}

func outcomeError(outcome CloseOutcome) error {
	var result error
	if !outcome.Released {
		result = ErrUnconfirmedRelease
	}
	for _, failure := range outcome.Failures {
		result = errors.Join(result, fmt.Errorf("frame: close %s: %s", failure.Resource, failure.Detail))
	}
	return result
}

// A parent resource records cleanup failures, not the child's temporary lack
// of release evidence. Late proof must be able to produce a clean final close.
func withoutUnconfirmedState(err error) error {
	if err == nil || err == ErrUnconfirmedRelease {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var failures []error
		for _, failure := range joined.Unwrap() {
			failures = append(failures, withoutUnconfirmedState(failure))
		}
		return errors.Join(failures...)
	}
	return err
}

func (c *Close) Outcome() CloseOutcome {
	c.lifetime.mu.Lock()
	defer c.lifetime.mu.Unlock()
	return c.lifetime.outcomeLocked(c.id)
}

func (c *Close) Wait(ctx context.Context) (CloseOutcome, error) {
	if ctx == nil {
		return CloseOutcome{}, ErrInvalid
	}
	select {
	case <-c.settled:
		outcome := c.Outcome()
		return outcome, outcomeError(outcome)
	default:
	}
	select {
	case <-c.settled:
		outcome := c.Outcome()
		return outcome, outcomeError(outcome)
	case <-ctx.Done():
		select {
		case <-c.settled:
			outcome := c.Outcome()
			return outcome, outcomeError(outcome)
		default:
		}
		return c.Outcome(), contextError(ctx)
	}
}

func (c *Close) WaitReleased(ctx context.Context) (CloseOutcome, error) {
	if ctx == nil {
		return CloseOutcome{}, ErrInvalid
	}
	for {
		l := c.lifetime
		l.mu.Lock()
		outcome := l.outcomeLocked(c.id)
		changed := l.changed
		l.mu.Unlock()
		if outcome.Released {
			return outcome, outcomeError(outcome)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			outcome = c.Outcome()
			if outcome.Released {
				return outcome, outcomeError(outcome)
			}
			return outcome, contextError(ctx)
		}
	}
}

func (l *Lifetime) Close(ctx context.Context) (CloseOutcome, error) {
	return l.BeginClose(ErrClosed).Wait(ctx)
}
