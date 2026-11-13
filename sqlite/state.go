package sqlite

import (
	"errors"

	framehost "github.com/Tangerg/frame/host"
)

type submissionState string

const (
	submissionPrepared              submissionState = "prepared"
	submissionCommitted             submissionState = submissionState(framehost.CommitSucceeded)
	submissionRejected              submissionState = submissionState(framehost.CommitRejected)
	submissionCommittedSynchronized submissionState = "committed_synchronized"
	submissionRejectedSynchronized  submissionState = "not_committed_synchronized"
)

// Prepared is durable intent, not a terminal business conclusion. Unknown
// stored states are corruption rather than another spelling of that intent.
func (s submissionState) settlement() (framehost.CommitState, error) {
	switch s {
	case submissionPrepared:
		return framehost.CommitIndeterminate, nil
	case submissionCommitted, submissionCommittedSynchronized:
		return framehost.CommitSucceeded, nil
	case submissionRejected, submissionRejectedSynchronized:
		return framehost.CommitRejected, nil
	default:
		return framehost.CommitIndeterminate, errors.Join(ErrIndeterminate, errors.New("sqlite: invalid stored submission state"))
	}
}

func (s submissionState) synchronized() bool {
	return s == submissionCommittedSynchronized || s == submissionRejectedSynchronized
}

func (s submissionState) confirm() (submissionState, error) {
	switch s {
	case submissionCommitted, submissionCommittedSynchronized:
		return submissionCommittedSynchronized, nil
	case submissionRejected, submissionRejectedSynchronized:
		return submissionRejectedSynchronized, nil
	case submissionPrepared:
		return s, ErrUnsettled
	default:
		return s, errors.Join(ErrIndeterminate, errors.New("sqlite: invalid stored submission state"))
	}
}

// Exit may be proven before the runner acknowledges it, so handoff must
// survive reconstruction without resurrecting execution protection.
type AttemptState string

const (
	AttemptApproved AttemptState = "approved"
	AttemptHandoff  AttemptState = "handoff"
	AttemptReleased AttemptState = "released"
)

func (s AttemptState) pending() bool {
	return s == AttemptApproved || s == AttemptHandoff
}

func (s AttemptState) validate() error {
	switch s {
	case AttemptApproved, AttemptHandoff, AttemptReleased:
		return nil
	default:
		return errors.New("sqlite: invalid stored attempt state")
	}
}

func (s AttemptState) afterProof(target AttemptState) (AttemptState, error) {
	if err := s.validate(); err != nil {
		return s, err
	}
	if target != AttemptHandoff && target != AttemptReleased {
		return s, errors.New("sqlite: invalid attempt proof transition")
	}
	if s == AttemptReleased || s == target {
		return s, nil
	}
	return target, nil
}

type useState string

const (
	useApproved useState = "approved"
	useReleased useState = "released"
)

func (s useState) validate() error {
	switch s {
	case useApproved, useReleased:
		return nil
	default:
		return errors.New("sqlite: invalid stored use state")
	}
}
