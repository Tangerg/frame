package process

import "github.com/Tangerg/frame/sqlite"

const (
	runnerWaitProofKind    = "runner-wait"
	undispatchedProofKind  = "not-dispatched"
	sealedAbsenceProofKind = "sealed-not-started"
	undispatchedExitSuffix = ":" + undispatchedProofKind
	undispatchedEvidence   = `{"dispatched":false}`
)

func undispatchedProof(attempt sqlite.Attempt) sqlite.ExitProof {
	return sqlite.ExitProof{
		AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: attempt.ID + undispatchedExitSuffix,
		Kind: undispatchedProofKind, Evidence: []byte(undispatchedEvidence),
	}
}
