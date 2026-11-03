package sqlite

import (
	"errors"

	"github.com/Tangerg/frame"
)

type submissionState string

const (
	submissionPrepared     submissionState = "prepared"
	submissionCommitted    submissionState = submissionState(frame.CommitSucceeded)
	submissionNotCommitted submissionState = submissionState(frame.CommitRejected)
)

// Prepared is durable intent, not a terminal business conclusion. Unknown
// stored states are corruption rather than another spelling of that intent.
func (s submissionState) settlement() (frame.CommitState, error) {
	switch s {
	case submissionPrepared:
		return frame.CommitIndeterminate, nil
	case submissionCommitted:
		return frame.CommitSucceeded, nil
	case submissionNotCommitted:
		return frame.CommitRejected, nil
	default:
		return frame.CommitIndeterminate, errors.New("sqlite: invalid stored submission state")
	}
}

// AttemptState is the durable progress of an execution attempt: approved to
// run, exit proven but its runner not yet acknowledged, or fully released.
type AttemptState string

const (
	AttemptApproved AttemptState = "approved"
	AttemptHandoff  AttemptState = "handoff"
	AttemptReleased AttemptState = "released"
)

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
