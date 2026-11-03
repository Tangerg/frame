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
	"weak"
)

type ReleaseState string

const (
	ResourceReleased    ReleaseState = "released"
	ResourceUnconfirmed ReleaseState = "unconfirmed"
)

var (
	ErrUnconfirmedRelease = errors.New("frame: resource release unconfirmed")
	ErrCloseFailed        = errors.New("frame: resource cleanup failed")
)

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
	resource     Resource
	dependencies []*resourceEntry
	consumers    int
	index        int
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
	closing         *Close
	released        bool
	causes          weak.Pointer[CloseCauses]
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
	ready       []*resourceEntry
	running     int
}

var closeSequence atomic.Uint64

const (
	defaultLifetimeWork      = 4096
	defaultLifetimeResources = 4096
)

func NewLifetime(parent context.Context) *Lifetime {
	lifetime, err := NewLifetimeWithLimits(parent, LifetimeLimits{})
	if err != nil {
		panic(err)
	}
	return lifetime
}

// Limits count resources until their original release is proved, including
// resources whose cleanup returned without a release conclusion.
func NewLifetimeWithLimits(parent context.Context, limits LifetimeLimits) (*Lifetime, error) {
	if parent == nil {
		return nil, ErrInvalid
	}
	if limits.MaxWork == 0 {
		limits.MaxWork = defaultLifetimeWork
	}
	if limits.MaxResources == 0 {
		limits.MaxResources = defaultLifetimeResources
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

func (l *Lifetime) isClosing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closing != nil || l.released || l.ctx.Err() != nil
}

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
			l.recordFailureLocked(resource.Name, detail, cancelErr)
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
	entry := &resourceEntry{resource: resource, index: len(l.resources)}
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
	for _, dependency := range entry.dependencies {
		dependency.consumers++
	}
	l.resources = append(l.resources, entry)
	l.signalLocked()
	return &ResourceHandle{lifetime: l, entry: entry}, nil
}

func cleanupResource(name string, closeResource func() error) Resource {
	resource := Resource{Name: name}
	if closeResource != nil {
		resource.Close = func(context.Context) (ReleaseState, error) {
			if err := closeResource(); err != nil {
				return ResourceUnconfirmed, err
			}
			return ResourceReleased, nil
		}
	}
	return resource
}

func (l *Lifetime) Cleanup(name string, closeResource func() error, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	return l.Own(cleanupResource(name, closeResource), dependencies...)
}
func (w *Work) Cleanup(name string, closeResource func() error, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	return w.Own(cleanupResource(name, closeResource), dependencies...)
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
			l.recordFailureLocked("cancellation", detail, err)
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
	l.mu.Lock()
	operation.ready = make([]*resourceEntry, 0, len(l.resources))
	for _, entry := range l.resources {
		l.enqueueResourceLocked(entry)
	}
	l.mu.Unlock()
	for {
		l.mu.Lock()
		ready := operation.ready
		operation.ready = []*resourceEntry{}
		for _, entry := range ready {
			if !entry.released {
				entry.attempted = true
				entry.running = true
				operation.running++
			}
		}
		if len(l.resources) == 0 {
			l.released = true
			operation.ready = nil
			l.resources = nil
			l.work = nil
			l.causes = weak.Pointer[CloseCauses]{}
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
		running := operation.running
		l.mu.Unlock()
		if running == 0 {
			operation.settledOnce.Do(func() { close(operation.settled) })
		}
		for _, entry := range ready {
			l.mu.Lock()
			closeResource := entry.resource.Close
			l.mu.Unlock()
			if closeResource != nil {
				go l.closeResource(entry, closeResource)
			}
		}
		<-changed
	}
}

func (l *Lifetime) enqueueResourceLocked(entry *resourceEntry) {
	if l.closing != nil && l.closing.ready != nil && entry.consumers == 0 && !entry.attempted && !entry.released {
		l.closing.ready = append(l.closing.ready, entry)
	}
}

// Only the original release transition updates both the graph and its ready
// index. A running cleanup keeps its dependency holds even after late proof.
func (l *Lifetime) forgetResourceLocked(entry *resourceEntry) {
	for _, dependency := range entry.dependencies {
		dependency.consumers--
		l.enqueueResourceLocked(dependency)
	}
	entry.resource = Resource{Name: entry.resource.Name}
	entry.dependencies = nil
	last := len(l.resources) - 1
	l.resources[entry.index] = l.resources[last]
	l.resources[entry.index].index = entry.index
	l.resources[last] = nil
	l.resources = l.resources[:last]
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
	l.closing.running--
	if state == ResourceReleased {
		entry.released = true
	}
	if err != nil {
		l.recordFailureLocked(entry.resource.Name, detail, err)
	}
	if entry.released {
		l.forgetResourceLocked(entry)
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
	return boundedDetail(fmt.Sprint(err), maxDetailBytes)
}

func boundedDetail(detail string, maxDetailBytes int) string {
	detail = strings.ToValidUTF8(detail, "\uFFFD")
	if len(detail) > maxDetailBytes {
		detail = detail[:maxDetailBytes]
		for !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
		return strings.Clone(detail)
	}
	return detail
}

func (l *Lifetime) recordFailureLocked(name, detail string, cause error) {
	if observation := l.causes.Value(); observation != nil {
		observation.record(name, cause)
	}
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
		l.recordFailureLocked(h.entry.resource.Name, detail, diagnostic)
	}
	if !h.entry.running {
		l.forgetResourceLocked(h.entry)
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
	failed := len(outcome.Failures) > 0 || outcome.OmittedFailures > 0
	if !outcome.Released {
		if failed {
			return errors.Join(ErrUnconfirmedRelease, ErrCloseFailed)
		}
		return ErrUnconfirmedRelease
	}
	if failed {
		return ErrCloseFailed
	}
	return nil
}

// A parent resource records cleanup failures, not the child's temporary lack
// of release evidence. Late proof must be able to produce a clean final close.
func withoutUnconfirmedState(err error) error {
	if err == nil || err == ErrUnconfirmedRelease {
		return nil
	}
	if local, ok := err.(*localCloseError); ok {
		return &localCloseError{status: withoutUnconfirmedState(local.status), causes: local.causes}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var failures []error
		for _, failure := range joined.Unwrap() {
			if remaining := withoutUnconfirmedState(failure); remaining != nil {
				failures = append(failures, remaining)
			}
		}
		if len(failures) == 1 {
			return failures[0]
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

// Wait reports bounded evidence from the original close. Resource causes belong
// to an explicit CloseCauses observation, so retaining this terminal operation
// cannot retain arbitrary plugin objects through an error chain.
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
	causes := l.ObserveCloseCauses()
	outcome, err := l.BeginClose(ErrClosed).Wait(ctx)
	return outcome, causes.join(err)
}
