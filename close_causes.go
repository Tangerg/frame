package frame

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"weak"
)

// CloseCauses is a local, caller-owned observation of original resource errors.
// Keeping it alive opts into retaining those errors and anything they reference.
// Lifetime stores only a weak reference and bounded diagnostic projections.
type CloseCauses struct {
	mu     sync.Mutex
	causes []*closeCause
	report *closeCauseReport
}

type closeCause struct {
	resource string
	cause    error
}

func (e *closeCause) Error() string { return fmt.Sprintf("frame: close %s: %v", e.resource, e.cause) }
func (e *closeCause) Unwrap() error { return e.cause }

type closeCauseReport struct {
	causes []*closeCause
}

func (e *closeCauseReport) Error() string { return errors.Join(e.Unwrap()...).Error() }
func (e *closeCauseReport) Unwrap() []error {
	causes := make([]error, len(e.causes))
	for i, cause := range e.causes {
		causes[i] = cause
	}
	return causes
}

type localCloseError struct {
	status error
	causes *closeCauseReport
}

func (e *localCloseError) Error() string { return errors.Join(e.status, e.causes).Error() }
func (e *localCloseError) Unwrap() []error {
	if e.status == nil {
		return []error{e.causes}
	}
	return []error{e.status, e.causes}
}

// ObserveCloseCauses must precede the failures of interest. It shares the live
// observation, without reconstructing past causes from diagnostic strings.
func (l *Lifetime) ObserveCloseCauses() *CloseCauses {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.released {
		if current := l.causes.Value(); current != nil {
			return current
		}
	}
	observation := &CloseCauses{}
	if !l.released {
		l.causes = weak.Make(observation)
	}
	return observation
}

func (c *CloseCauses) snapshot() *closeCauseReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.causes) == 0 {
		return nil
	}
	if c.report == nil {
		c.report = &closeCauseReport{causes: slices.Clone(c.causes)}
	}
	return c.report
}

func (c *CloseCauses) Err() error {
	if report := c.snapshot(); report != nil {
		return report
	}
	return nil
}

func (c *CloseCauses) join(status error) error {
	report := c.snapshot()
	if report == nil {
		return status
	}
	if existing, ok := status.(*localCloseError); ok && existing.causes == report {
		return existing
	}
	return &localCloseError{status: status, causes: report}
}

// Parent callbacks and release proofs can observe the same child failure. Copy
// its original leaf identities, so nesting owners cannot multiply error trees.
func (c *CloseCauses) record(resource string, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var inherited *closeCauseReport
	switch original := cause.(type) {
	case *localCloseError:
		inherited = original.causes
		cause = original.status
		if cause == ErrCloseFailed || cause == ErrUnconfirmedRelease {
			cause = nil
		}
	case *closeCauseReport:
		inherited = original
		cause = nil
	}
	if inherited != nil {
		for _, original := range inherited.causes {
			if !slices.Contains(c.causes, original) {
				c.causes = append(c.causes, original)
				c.report = nil
			}
		}
	}
	if cause != nil {
		c.causes = append(c.causes, &closeCause{resource: resource, cause: cause})
		c.report = nil
	}
}
