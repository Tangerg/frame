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

func (e *closeCauseReport) merge(other *closeCauseReport) *closeCauseReport {
	if e == nil {
		return other
	}
	if other == nil || other == e {
		return e
	}
	causes := slices.Clone(e.causes)
	for _, original := range other.causes {
		if !slices.Contains(causes, original) {
			causes = append(causes, original)
		}
	}
	if len(causes) == len(e.causes) {
		return e
	}
	return &closeCauseReport{causes: causes}
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
	status     closeStatus
	additional []error
	causes     *closeCauseReport
}

func newLocalCloseError(status closeStatus, additional []error, causes *closeCauseReport) error {
	if causes == nil && len(additional) == 0 {
		return status.err()
	}
	if causes == nil && status == 0 && len(additional) == 1 {
		return additional[0]
	}
	return &localCloseError{status: status, additional: additional, causes: causes}
}

func (e *localCloseError) Error() string { return errors.Join(e.Unwrap()...).Error() }
func (e *localCloseError) Unwrap() []error {
	var causes []error
	if status := e.status.err(); status != nil {
		causes = append(causes, status)
	}
	if e.causes != nil {
		causes = append(causes, e.causes)
	}
	return append(causes, e.additional...)
}

// ObserveCloseCauses must precede the failures of interest. It shares the live
// observation, without reconstructing past causes from diagnostic strings.
func (l *lifetimeRecord) ObserveCloseCauses() *CloseCauses {
	if l == nil {
		return nil
	}
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
	if c == nil {
		return nil
	}
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
	if c == nil {
		return ErrInvalid
	}
	if report := c.snapshot(); report != nil {
		return report
	}
	return nil
}

// Join preserves original failure identities across close compositions. A
// conclusion or an already observed failure cannot become another occurrence.
func (c *CloseCauses) Join(observed ...error) error {
	if c == nil {
		return errors.Join(append([]error{ErrInvalid}, observed...)...)
	}
	var status closeStatus
	var additional []error
	causes := c.snapshot()
	for _, err := range observed {
		switch original := err.(type) {
		case nil:
		case closeConclusion:
			status |= original.status
		case *localCloseError:
			status |= original.status
			additional = append(additional, original.additional...)
			causes = causes.merge(original.causes)
		case *closeCauseReport:
			causes = causes.merge(original)
		default:
			additional = append(additional, err)
		}
	}
	return newLocalCloseError(status, additional, causes)
}

// Parent callbacks and release proofs can observe the same child failure. Copy
// its original leaf identities, so nesting owners cannot multiply error trees.
func (c *CloseCauses) record(resource string, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var inherited *closeCauseReport
	var additional []error
	switch original := cause.(type) {
	case *localCloseError:
		inherited = original.causes
		additional = original.additional
	case *closeCauseReport:
		inherited = original
	case closeConclusion:
	default:
		additional = []error{cause}
	}
	if inherited != nil {
		for _, original := range inherited.causes {
			if !slices.Contains(c.causes, original) {
				c.causes = append(c.causes, original)
				c.report = nil
			}
		}
	}
	for _, failure := range additional {
		c.causes = append(c.causes, &closeCause{resource: resource, cause: failure})
		c.report = nil
	}
}
