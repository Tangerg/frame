package process

import "github.com/Tangerg/frame/sqlite"

const (
	runnerWaitProofKind    = "runner-wait"
	undispatchedProofKind  = "not-dispatched"
	sealedAbsenceProofKind = "sealed-not-started"
	undispatchedEvidence   = `{"dispatched":false}`
)

func undispatchedProof(attempt sqlite.Attempt) sqlite.ExitProof {
	return attempt.LocalExit(undispatchedProofKind, undispatchedEvidence)
}
