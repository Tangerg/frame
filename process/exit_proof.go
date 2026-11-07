package process

import (
	json "encoding/json/v2"
	"math"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

const (
	runnerWaitProofKind       = "runner-wait"
	runnerNotCreatedProofKind = "runner-not-created"
	undispatchedProofKind     = "not-dispatched"
	sealedAbsenceProofKind    = "sealed-not-started"
	undispatchedEvidence      = `{"dispatched":false}`
)

func undispatchedProof(attempt sqlite.Attempt) sqlite.ExitProof {
	return attempt.LocalExit(undispatchedProofKind, undispatchedEvidence)
}

// Runner diagnostics remain with its original observation. Durable handoff
// retains only the immutable identity that the runner itself acknowledges.
func runnerExitProof(attempt sqlite.Attempt, receipt runner.ExitReceipt) (sqlite.ExitProof, error) {
	evidence, err := json.Marshal(receipt)
	return sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: receipt.ProofID, Kind: runnerProofKind(receipt.State), Evidence: evidence}, err
}

func runnerProofKind(state runner.State) string {
	if state == runner.NotCreated {
		return runnerNotCreatedProofKind
	}
	return runnerWaitProofKind
}

func absenceExitProof(attempt sqlite.Attempt, proof runner.AbsenceProof) (sqlite.ExitProof, error) {
	evidence, err := json.Marshal(proof)
	return sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: proof.ProofID, Kind: sealedAbsenceProofKind, Evidence: evidence}, err
}

func (d *Driver) checkExitCapacity(attempt sqlite.Attempt) error {
	if err := d.config.Store.CheckExitCapacity(undispatchedProof(attempt)); err != nil {
		return err
	}
	// HTML-sensitive bytes maximize the stored proof identity's JSON expansion.
	receipt := runner.ExitReceipt{AttemptID: attempt.ID, Epoch: attempt.Epoch, RunnerID: attempt.Realm, ProofID: strings.Repeat("<", frame.MaxNameBytes), State: runner.NotCreated}
	exit, err := runnerExitProof(attempt, receipt)
	if err != nil {
		return err
	}
	if err := d.config.Store.CheckExitCapacity(exit); err != nil {
		return err
	}
	absence := runner.AbsenceProof{AttemptID: attempt.ID, Epoch: attempt.Epoch, RunnerID: attempt.Realm, FenceEpoch: math.MaxUint64, ProofID: value.Hash(nil).String()}
	exit, err = absenceExitProof(attempt, absence)
	if err != nil {
		return err
	}
	return d.config.Store.CheckExitCapacity(exit)
}
