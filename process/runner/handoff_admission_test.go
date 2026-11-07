package runner

import (
	"errors"
	"testing"
)

func TestExitReceiptRequiresAnAdmittedTerminalObservation(t *testing.T) {
	ref := AttemptRef{AttemptID: "original", Epoch: 1, RunnerID: "runner"}
	for _, test := range []struct {
		name     string
		state    State
		pid      int
		exitCode *int
		valid    bool
	}{
		{name: "not-created", state: NotCreated, valid: true},
		{name: "original-wait", state: Exited, pid: 42, exitCode: new(0), valid: true},
		{name: "not-created-with-process", state: NotCreated, pid: 42},
		{name: "not-created-with-exit-code", state: NotCreated, exitCode: new(0)},
		{name: "exited-without-process", state: Exited, exitCode: new(0)},
		{name: "exited-without-wait", state: Exited, pid: 42},
		{name: "exited-without-either", state: Exited},
		{name: "unknown-with-proof", state: Unknown},
		{name: "running-with-proof", state: Running, pid: 42},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := Observation{AttemptID: ref.AttemptID, Epoch: ref.Epoch, RunnerID: ref.RunnerID, State: test.state, PID: test.pid, ExitCode: test.exitCode, ProofID: "original-proof"}
			receipt, err := observation.ExitReceipt()
			if test.valid {
				if err != nil || !receipt.matches(observation) || receipt.State != test.state {
					t.Fatalf("valid terminal observation lost its proof: %+v %v", receipt, err)
				}
			} else if !errors.Is(err, ErrUnknown) || receipt != (ExitReceipt{}) {
				t.Fatalf("illegal terminal observation acquired release authority: %+v %v", receipt, err)
			}
		})
	}
}
