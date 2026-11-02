package sqlite

import "errors"

type submissionState string

const (
	submissionPrepared     submissionState = "prepared"
	submissionCommitted    submissionState = submissionState(Committed)
	submissionNotCommitted submissionState = submissionState(NotCommitted)
)

// Prepared is durable intent, not a terminal business conclusion. Unknown
// stored states are corruption rather than another spelling of that intent.
func (s submissionState) settlement() (SettlementState, error) {
	switch s {
	case submissionPrepared:
		return Indeterminate, nil
	case submissionCommitted:
		return Committed, nil
	case submissionNotCommitted:
		return NotCommitted, nil
	default:
		return Indeterminate, errors.New("sqlite: invalid stored submission state")
	}
}

type attemptState string

const (
	attemptApproved attemptState = "approved"
	attemptHandoff  attemptState = "handoff"
	attemptReleased attemptState = "released"
)

func (s attemptState) validate() error {
	switch s {
	case attemptApproved, attemptHandoff, attemptReleased:
		return nil
	default:
		return errors.New("sqlite: invalid stored attempt state")
	}
}

func (s attemptState) afterProof(target attemptState) (attemptState, error) {
	if err := s.validate(); err != nil {
		return s, err
	}
	if target != attemptHandoff && target != attemptReleased {
		return s, errors.New("sqlite: invalid attempt proof transition")
	}
	if s == attemptReleased || s == target {
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
