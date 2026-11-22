package frame

import "errors"

type CloseFailure struct {
	Resource string `json:"resource"`
	Detail   string `json:"detail"`
}

// CloseOutcome is bounded close evidence. Pending names the resources still
// awaiting release proof; ActiveWork counts intervals that have not finished.
type CloseOutcome struct {
	ID              uint64         `json:"id"`
	Released        bool           `json:"released"`
	Pending         []string       `json:"pending"`
	ActiveWork      int            `json:"activeWork"`
	Failures        []CloseFailure `json:"failures"`
	OmittedFailures uint64         `json:"omittedFailures"`
}

// ReleaseProof carries only the original close identity: diagnostics cannot
// enlarge or revoke evidence that every resource has released.
type ReleaseProof struct {
	CloseID uint64 `json:"closeId"`
}

// A close conclusion is a projection, never another resource failure. Its
// concrete type keeps parent composition from reinterpreting it as a cause.
type closeStatus uint8

type closeConclusion struct{ status closeStatus }

const (
	closeUnconfirmed closeStatus = 1 << iota
	closeFailed
)

func (s closeStatus) err() error {
	if s == 0 {
		return nil
	}
	return closeConclusion{status: s}
}

func (c closeConclusion) Error() string { return errors.Join(c.Unwrap()...).Error() }

func (c closeConclusion) Unwrap() []error {
	var status []error
	if c.status&closeUnconfirmed != 0 {
		status = append(status, ErrUnconfirmedRelease)
	}
	if c.status&closeFailed != 0 {
		status = append(status, ErrCloseFailed)
	}
	return status
}

func (outcome CloseOutcome) Err() error {
	var status closeStatus
	if !outcome.Released {
		status |= closeUnconfirmed
	}
	if len(outcome.Failures) > 0 || outcome.OmittedFailures > 0 {
		status |= closeFailed
	}
	return status.err()
}

// Only Frame's own conclusion may lose its temporary unconfirmed category.
// An opaque error is an actual failure, even if it wraps that same category.
func (outcome CloseOutcome) ResourceState(err error) (ReleaseState, error) {
	if outcome.Released {
		return ResourceReleased, err
	}
	switch original := err.(type) {
	case closeConclusion:
		err = (original.status &^ closeUnconfirmed).err()
	case *localCloseError:
		err = newLocalCloseError(original.status&^closeUnconfirmed, original.additional, original.causes)
	}
	return ResourceUnconfirmed, err
}

var (
	ErrUnconfirmedRelease = errors.New("frame: resource release unconfirmed")
	ErrCloseFailed        = errors.New("frame: resource cleanup failed")
)
