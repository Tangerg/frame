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

func (ReleaseState) Values() []string {
	return []string{string(ResourceReleased), string(ResourceUnconfirmed)}
}

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

// ReleaseProof carries only the original close identity: diagnostics cannot
// enlarge or revoke evidence that every resource has released.
type ReleaseProof struct {
	CloseID uint64 `json:"closeId"`
}

// cleanupState is the progress of a resource's own Close callback. Release
// proof is a separate fact: it may arrive before, during or after cleanup.
type cleanupState uint8

const (
	cleanupPending cleanupState = iota
	cleanupRunning
	cleanupFinished
)

// resourceEntry is a node of the release graph: it closes only after every
// consumer that depends on it has been released.
type resourceEntry struct {
	resource     Resource
	dependencies []*resourceEntry
	consumers    int
	index        int
	cleanup      cleanupState
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
	// Host-created lifetimes stay inside the admission domain that can inspect
	// their provider dependencies. Application lifetimes have no such affinity.
	authority       *authorityIdentity
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
	closing         *Close
	released        bool
	causes          weak.Pointer[CloseCauses]
	failures        []CloseFailure
	omittedFailures uint64
}

// Close is one owned retirement, independent of all callers waiting for it.
// It settles once no cleanup is running, even if some release is unconfirmed.
type Close struct {
	lifetime    *Lifetime
	id          uint64
	settled     chan struct{}
	settledOnce sync.Once
	// releasing starts after all work has drained; ready then holds the
	// resources whose consumers have all been released.
	releasing bool
	ready     []*resourceEntry
	running   int
}

func (c *Close) settle() { c.settledOnce.Do(func() { close(c.settled) }) }

var closeSequence atomic.Uint64

const (
	defaultLifetimeWork      = 4096
	defaultLifetimeResources = 4096
	// Close evidence is retained in terminal handles and wire reports, so its
	// size cannot grow with the number or verbosity of failing resources.
	maxCloseFailures        = 32
	maxCloseDiagnosticBytes = 2048
	cancellationResource    = "cancellation"
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
	if err := resolveLimit("lifetime work", &limits.MaxWork, defaultLifetimeWork); err != nil {
		return nil, err
	}
	if err := resolveLimit("lifetime resources", &limits.MaxResources, defaultLifetimeResources); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	l := &Lifetime{ctx: ctx, cancelContext: cancel, parentDone: parent.Done(), changed: make(chan struct{}), work: make(map[*workInterval]struct{}), limits: limits}
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
	return l.closingLocked()
}

func (l *Lifetime) closingLocked() bool {
	// The parent's cancellation is already authoritative while its AfterFunc
	// notification is still queued. The notification only starts retirement.
	return l.closing != nil || l.released || l.ctx.Err() != nil || isClosed(l.parentDone)
}

func (l *Lifetime) closeOutcome() (CloseOutcome, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing == nil {
		return CloseOutcome{}, false
	}
	return l.outcomeLocked(l.closing.id), true
}

func (l *Lifetime) cancel(cause error) {
	l.mu.Lock()
	cancel := l.cancelContext
	l.mu.Unlock()
	cancel(cause)
}

func (l *Lifetime) signalLocked() { close(l.changed); l.changed = make(chan struct{}) }

func (l *Lifetime) Own(resource Resource, dependencies ...*ResourceHandle) (*ResourceHandle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closingLocked() {
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
	resource.Name = boundedDiagnostic(resource.Name)
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
	work := make([]*workInterval, 0, len(l.work))
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
	l.deliverCancellations(cancellations, cause)
	l.awaitWork()
	l.releaseResources(operation)
}

func (l *Lifetime) deliverCancellations(cancellations []func(error), cause error) {
	for _, cancel := range cancellations {
		if err := invokeCancel(cancel, cause); err != nil {
			detail := closeFailureDetail(err)
			l.mu.Lock()
			l.recordFailureLocked(cancellationResource, detail, err)
			l.mu.Unlock()
		}
	}
}

func (l *Lifetime) awaitWork() {
	for {
		l.mu.Lock()
		idle, changed := len(l.work) == 0, l.changed
		l.mu.Unlock()
		if idle {
			return
		}
		<-changed
	}
}

// releaseResources closes resources in dependency order, each as soon as its
// consumers have been released, until every release is proved.
func (l *Lifetime) releaseResources(operation *Close) {
	l.mu.Lock()
	operation.releasing = true
	for _, entry := range l.resources {
		l.enqueueResourceLocked(entry)
	}
	l.mu.Unlock()
	for {
		l.mu.Lock()
		if len(l.resources) == 0 {
			stop := l.releaseLocked()
			l.mu.Unlock()
			if stop != nil {
				stop()
			}
			operation.settle()
			return
		}
		ready := operation.startReadyLocked()
		changed, running := l.changed, operation.running
		l.mu.Unlock()
		if running == 0 {
			operation.settle()
		}
		for _, entry := range ready {
			go l.closeResource(entry)
		}
		<-changed
	}
}

func (c *Close) startReadyLocked() []*resourceEntry {
	ready := c.ready
	c.ready = nil
	started := ready[:0]
	for _, entry := range ready {
		if entry.released {
			continue
		}
		entry.cleanup = cleanupRunning
		c.running++
		started = append(started, entry)
	}
	if len(ready) > 0 {
		c.lifetime.signalLocked()
	}
	return started
}

func (l *Lifetime) releaseLocked() func() bool {
	l.released = true
	l.resources = nil
	l.work = nil
	l.causes = weak.Pointer[CloseCauses]{}
	terminal, cancel := context.WithCancelCause(context.Background())
	cancel(ErrClosed)
	l.ctx = terminal
	l.cancelContext = cancel
	l.parentDone = nil
	l.signalLocked()
	stop := l.stopParent
	l.stopParent = nil
	return stop
}

func (l *Lifetime) enqueueResourceLocked(entry *resourceEntry) {
	if l.closing != nil && l.closing.releasing && entry.consumers == 0 && entry.cleanup == cleanupPending && !entry.released {
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

func callbackPanicError(callback string, value any) error {
	// Formatting an arbitrary panic value could reenter plugin code or Goexit.
	diagnostic := fmt.Errorf("frame: %s panicked", callback)
	if cause, ok := value.(error); ok {
		return errors.Join(diagnostic, cause)
	}
	return diagnostic
}

func invokeCancel(cancel func(error), cause error) error {
	return joinCallback("resource cancellation", func() error {
		cancel(cause)
		return nil
	})
}

func joinCallback(name string, callback func() error) error {
	// Goexit cannot be recovered. Join the original callback's goroutine so
	// its exit cannot abandon the caller's remaining cleanup or settlement.
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = fmt.Errorf("frame: %s exited without returning", name)
		defer func() {
			if value := recover(); value != nil {
				err = callbackPanicError(name, value)
			}
		}()
		err = callback()
	}()
	<-done
	return err
}

func invokeResourceClose(closeResource func(context.Context) (ReleaseState, error), ctx context.Context) (state ReleaseState, err error) {
	state = ResourceUnconfirmed
	defer func() {
		if value := recover(); value != nil {
			state = ResourceUnconfirmed
			err = callbackPanicError("resource close", value)
		}
	}()
	state, err = closeResource(ctx)
	if state != ResourceReleased && state != ResourceUnconfirmed {
		return ResourceUnconfirmed, errors.Join(err, fmt.Errorf("%w: resource close state", ErrInvalid))
	}
	return state, err
}

func (l *Lifetime) closeResource(entry *resourceEntry) {
	l.mu.Lock()
	closeResource := entry.resource.Close
	ctx := context.WithoutCancel(l.ctx)
	l.mu.Unlock()
	state := ResourceUnconfirmed
	err := errors.New("frame: resource close exited without returning")
	// Callback exit ends cleanup even without a return value. Release still
	// requires the original callback's conclusion or a ResourceHandle proof.
	defer func() {
		detail := closeFailureDetail(err)
		l.mu.Lock()
		entry.cleanup = cleanupFinished
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
	}()
	state, err = invokeResourceClose(closeResource, ctx)
}

// Error formatting may call back into its resource owner or call Goexit. Its
// projection cannot hold the state lock or abandon the original close evidence.
func closeFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	var detail string
	if failure := joinCallback("close diagnostic", func() error {
		detail = fmt.Sprint(err)
		return nil
	}); failure != nil {
		detail = failure.Error()
	}
	return boundedDiagnostic(detail)
}

func boundedDiagnostic(text string) string {
	text = strings.ToValidUTF8(text, string(utf8.RuneError))
	if len(text) > maxCloseDiagnosticBytes {
		text = text[:maxCloseDiagnosticBytes]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		return strings.Clone(text)
	}
	return text
}

func (l *Lifetime) recordFailureLocked(name, detail string, cause error) {
	if observation := l.causes.Value(); observation != nil {
		observation.record(name, cause)
	}
	if len(l.failures) == maxCloseFailures {
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
	if h.entry.cleanup != cleanupRunning {
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

// releaseState concludes a resource whose release is a nested close. A pending
// nested release is reported as unconfirmed, not as a cleanup failure.
func releaseState(outcome CloseOutcome, err error) (ReleaseState, error) {
	if outcome.Released {
		return ResourceReleased, err
	}
	return ResourceUnconfirmed, withoutUnconfirmedState(err)
}

func (c *Close) Outcome() CloseOutcome {
	c.lifetime.mu.Lock()
	defer c.lifetime.mu.Unlock()
	return c.lifetime.outcomeLocked(c.id)
}

func (c *Close) ReleaseProof() (ReleaseProof, error) {
	if c == nil || c.lifetime == nil {
		return ReleaseProof{}, ErrInvalid
	}
	c.lifetime.mu.Lock()
	defer c.lifetime.mu.Unlock()
	if !c.lifetime.released {
		return ReleaseProof{}, ErrUnconfirmedRelease
	}
	return ReleaseProof{CloseID: c.id}, nil
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
	case <-ctx.Done():
	}
	for {
		l := c.lifetime
		l.mu.Lock()
		outcome := l.outcomeLocked(c.id)
		// Late proof can unlock original dependencies after the first settlement.
		// Wait for that cleanup, but never wait on a stationary missing proof.
		idle := c.running == 0 && len(c.ready) == 0 && len(l.resources) > 0
		settled := isClosed(c.settled) && (l.released || idle)
		changed := l.changed
		l.mu.Unlock()
		if settled {
			return outcome, outcomeError(outcome)
		}
		if err := contextError(ctx); err != nil {
			return outcome, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
		}
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
