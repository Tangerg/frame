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

	framevalue "github.com/Tangerg/frame/value"
)

type ReleaseState string

const (
	ResourceReleased    ReleaseState = "released"
	ResourceUnconfirmed ReleaseState = "unconfirmed"
)

func (ReleaseState) JSONSchemaEnum() []string {
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
// restart cleanup or transfer release ownership to a different lifetime. The
// zero handle is unissued.
type ResourceHandle struct {
	lifetime *lifetimeRecord
	entry    *resourceEntry
}

type LifetimeLimits struct {
	MaxWork      int
	MaxResources int
}

// Copies share the owner; replacing a handle cannot redirect existing work.
type Lifetime struct{ *lifetimeRecord }

// Identity permits dependency inspection without granting control of the lifetime.
type LifetimeID struct{ owner *lifetimeRecord }

func (l *lifetimeRecord) Identity() LifetimeID { return LifetimeID{l} }
func (h ResourceHandle) OwnerID() LifetimeID   { return LifetimeID{h.lifetime} }

func (h ResourceHandle) Valid() bool { return h.entry != nil }

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

// Close is one owned retirement, independent of all callers waiting for it.
// It settles once no cleanup is running, even if some release is unconfirmed.
// Copies observe the original operation without copying its mutable progress.
type Close struct{ *closeOperation }

type closeOperation struct {
	lifetime    *lifetimeRecord
	id          uint64
	settled     chan struct{}
	settledOnce sync.Once
	// releasing starts after all work has drained; ready then holds the
	// resources whose consumers have all been released.
	releasing bool
	ready     []*resourceEntry
	running   int
}

func (c *closeOperation) settle() { c.settledOnce.Do(func() { close(c.settled) }) }

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

func (l *lifetimeRecord) Own(resource Resource, dependencies ...ResourceHandle) (ResourceHandle, error) {
	if l == nil {
		return ResourceHandle{}, ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closingLocked() {
		return ResourceHandle{}, ErrClosed
	}
	return l.ownLocked(resource, dependencies)
}

func (l *lifetimeRecord) ownLocked(resource Resource, dependencies []ResourceHandle) (ResourceHandle, error) {
	if resource.Name == "" || resource.Close == nil {
		return ResourceHandle{}, ErrInvalid
	}
	if len(l.resources) >= l.limits.MaxResources {
		return ResourceHandle{}, fmt.Errorf("%w: lifetime resources", ErrCapacity)
	}
	resource.Name = BoundedDiagnostic(resource.Name)
	entry := &resourceEntry{resource: resource, index: len(l.resources)}
	seen := make(map[*resourceEntry]bool)
	for _, dependency := range dependencies {
		if !dependency.Valid() || dependency.lifetime != l || dependency.entry.released {
			return ResourceHandle{}, fmt.Errorf("%w: resource dependency", ErrInvalid)
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
	return ResourceHandle{lifetime: l, entry: entry}, nil
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

func (l *lifetimeRecord) Cleanup(name string, closeResource func() error, dependencies ...ResourceHandle) (ResourceHandle, error) {
	return l.Own(cleanupResource(name, closeResource), dependencies...)
}
func (l *lifetimeRecord) BeginClose(cause error) Close {
	if l == nil {
		return Close{}
	}
	if cause == nil {
		cause = ErrClosed
	}
	// Late acquisitions must see the original cause as soon as close is visible.
	l.Cancel(cause)
	l.mu.Lock()
	if l.closing != nil {
		result := l.closing
		l.mu.Unlock()
		return Close{result}
	}
	cause = context.Cause(l.ctx)
	operation := &closeOperation{lifetime: l, id: closeSequence.Add(1), settled: make(chan struct{})}
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
	for _, item := range work {
		item.cancel(cause)
	}
	go l.retire(operation, cancellations, cause)
	return Close{operation}
}

func (l *lifetimeRecord) retire(operation *closeOperation, cancellations []func(error), cause error) {
	l.deliverCancellations(cancellations, cause)
	l.awaitWork()
	l.releaseResources(operation)
}

func (l *lifetimeRecord) deliverCancellations(cancellations []func(error), cause error) {
	for _, cancel := range cancellations {
		if err := invokeCancel(cancel, cause); err != nil {
			detail := closeFailureDetail(err)
			l.mu.Lock()
			l.recordFailureLocked(cancellationResource, detail, err)
			l.mu.Unlock()
		}
	}
}

func (l *lifetimeRecord) awaitWork() {
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
func (l *lifetimeRecord) releaseResources(operation *closeOperation) {
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

func (c *closeOperation) startReadyLocked() []*resourceEntry {
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

func (l *lifetimeRecord) releaseLocked() func() bool {
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

func (l *lifetimeRecord) enqueueResourceLocked(entry *resourceEntry) {
	if l.closing != nil && l.closing.releasing && entry.consumers == 0 && entry.cleanup == cleanupPending && !entry.released {
		l.closing.ready = append(l.closing.ready, entry)
	}
}

// Only the original release transition updates both the graph and its ready
// index. A running cleanup keeps its dependency holds even after late proof.
func (l *lifetimeRecord) forgetResourceLocked(entry *resourceEntry) {
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

func CallbackPanicError(callback string, value any) error {
	// Formatting an arbitrary panic value could reenter plugin code or Goexit.
	diagnostic := fmt.Errorf("frame: %s panicked", callback)
	if cause, ok := value.(error); ok {
		return errors.Join(diagnostic, cause)
	}
	return diagnostic
}

func invokeCancel(cancel func(error), cause error) error {
	return InvokeCallback("resource cancellation", func() error {
		cancel(cause)
		return nil
	})
}

func InvokeCallback(name string, callback func() error) error {
	if callback == nil {
		return ErrInvalid
	}
	// Goexit cannot be recovered. Join the original callback's goroutine so
	// its exit cannot abandon the caller's remaining cleanup or settlement.
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = fmt.Errorf("frame: %s exited without returning", name)
		defer func() {
			if value := recover(); value != nil {
				err = CallbackPanicError(name, value)
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
			err = CallbackPanicError("resource close", value)
		}
	}()
	state, err = closeResource(ctx)
	if !slices.Contains(state.JSONSchemaEnum(), string(state)) {
		return ResourceUnconfirmed, errors.Join(err, fmt.Errorf("%w: resource close state", ErrInvalid))
	}
	return state, err
}

func (l *lifetimeRecord) closeResource(entry *resourceEntry) {
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
	if failure := InvokeCallback("close diagnostic", func() error {
		detail = fmt.Sprint(err)
		return nil
	}); failure != nil {
		detail = failure.Error()
	}
	return BoundedDiagnostic(detail)
}

func BoundedDiagnostic(text string) string {
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

func (l *lifetimeRecord) recordFailureLocked(name, detail string, cause error) {
	if observation := l.causes.Value(); observation != nil {
		observation.record(name, cause)
	}
	if len(l.failures) == maxCloseFailures {
		l.omittedFailures++
		return
	}
	l.failures = append(l.failures, CloseFailure{Resource: name, Detail: detail})
}

func (h ResourceHandle) ConfirmReleased(diagnostic error) error {
	if !h.Valid() {
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

func (l *lifetimeRecord) outcomeLocked(id uint64) CloseOutcome {
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

func (outcome CloseOutcome) Err() error {
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
func (outcome CloseOutcome) ResourceState(err error) (ReleaseState, error) {
	if outcome.Released {
		return ResourceReleased, err
	}
	return ResourceUnconfirmed, withoutUnconfirmedState(err)
}

func (c *closeOperation) Outcome() CloseOutcome {
	if c == nil {
		return CloseOutcome{}
	}
	c.lifetime.mu.Lock()
	defer c.lifetime.mu.Unlock()
	return c.lifetime.outcomeLocked(c.id)
}

func (c *closeOperation) ReleaseProof() (ReleaseProof, error) {
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
func (c *closeOperation) Wait(ctx context.Context) (CloseOutcome, error) {
	if c == nil || framevalue.IsNil(ctx) {
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
			return outcome, outcome.Err()
		}
		if err := ContextError(ctx); err != nil {
			return outcome, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
		}
	}
}

func (c *closeOperation) WaitReleased(ctx context.Context) (CloseOutcome, error) {
	if c == nil || framevalue.IsNil(ctx) {
		return CloseOutcome{}, ErrInvalid
	}
	for {
		l := c.lifetime
		l.mu.Lock()
		outcome := l.outcomeLocked(c.id)
		changed := l.changed
		l.mu.Unlock()
		if outcome.Released {
			return outcome, outcome.Err()
		}
		select {
		case <-changed:
		case <-ctx.Done():
			outcome = c.Outcome()
			if outcome.Released {
				return outcome, outcome.Err()
			}
			return outcome, ContextError(ctx)
		}
	}
}

func (l *lifetimeRecord) Close(ctx context.Context) (CloseOutcome, error) {
	if l == nil || framevalue.IsNil(ctx) {
		return CloseOutcome{}, ErrInvalid
	}
	causes := l.ObserveCloseCauses()
	outcome, err := l.BeginClose(ErrClosed).Wait(ctx)
	return outcome, causes.Join(err)
}

func isClosed(signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	default:
		return false
	}
}
