package process

import (
	json "encoding/json/v2"

	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
)

// Proof kinds name the evidence encoding the driver later decodes. A runner
// receipt carries its own terminal state, so one kind covers exit and refusal.
const (
	runnerExitProofKind    = "runner-exit"
	undispatchedProofKind  = "not-dispatched"
	sealedAbsenceProofKind = "sealed-not-started"
)

// undispatchedEvidence is the fixed document of an attempt whose creation
// request never left this driver.
const undispatchedEvidence = `{"dispatched":false}`

func undispatchedProof(attempt sqlite.Attempt) sqlite.ExitProof {
	return attempt.LocalExit(undispatchedProofKind, undispatchedEvidence)
}

// Runner diagnostics remain with its original observation. Durable handoff
// retains only the immutable identity that the runner itself acknowledges.
func runnerExitProof(attempt sqlite.Attempt, receipt runner.ExitReceipt) (sqlite.ExitProof, error) {
	evidence, err := json.Marshal(receipt)
	return sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: receipt.ProofID, Kind: runnerExitProofKind, Evidence: evidence}, err
}

func absenceExitProof(attempt sqlite.Attempt, proof runner.AbsenceProof) (sqlite.ExitProof, error) {
	evidence, err := json.Marshal(proof)
	return sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: proof.ProofID, Kind: sealedAbsenceProofKind, Evidence: evidence}, err
}
