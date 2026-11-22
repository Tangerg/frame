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

const (
	// Close evidence is retained in terminal handles and wire reports, so its
	// size cannot grow with the number or verbosity of failing resources.
	maxCloseFailures        = 32
	maxCloseDiagnosticBytes = 2048
	cancellationResource    = "cancellation"
)

// Close is one owned retirement, independent of all callers waiting for it.
// It settles once no cleanup is running, even if some release is unconfirmed.
// Copies observe the original operation without copying its mutable progress.
type Close struct{ *closeOperation }

// Valid reports whether the handle names a retirement; the zero Close does not.
func (c Close) Valid() bool { return c.closeOperation != nil }

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
	state, err = invokeResourceClose(ctx, closeResource)
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

func (l *lifetimeRecord) outcomeLocked(id uint64) CloseOutcome {
	outcome := CloseOutcome{ID: id, Released: l.released, ActiveWork: len(l.work), Failures: slices.Clone(l.failures), OmittedFailures: l.omittedFailures, Pending: []string{}}
	for _, entry := range l.resources {
		if !entry.released {
			outcome.Pending = append(outcome.Pending, entry.resource.Name)
		}
	}
	return outcome
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

var closeSequence atomic.Uint64

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
