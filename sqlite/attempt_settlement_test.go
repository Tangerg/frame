package sqlite

import (
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestAttemptSettlementJoinsOriginalApproval(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		statement string
		after     bool
		state     framehost.CommitState
		stored    bool
	}{
		{name: "committed", state: framehost.Committed, stored: true},
		{name: "begin refused", statement: "BEGIN IMMEDIATE", state: framehost.NotCommitted},
		{name: "begin acknowledgement lost", statement: "BEGIN IMMEDIATE", after: true, state: framehost.NotCommitted},
		{name: "commit refused", statement: "COMMIT", state: framehost.NotCommitted},
		{name: "commit acknowledgement lost", statement: "COMMIT", after: true, state: framehost.CommitIndeterminate, stored: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store, _, _ := testStore(t)
			attempt := testAttempt(store)
			failure := errors.New("original attempt acknowledgement lost")
			var injected atomic.Bool
			inject := func(statement string) error {
				if statement == scenario.statement && injected.CompareAndSwap(false, true) {
					return failure
				}
				return nil
			}
			fault := transactionFault{beforeExec: inject}
			if scenario.after {
				fault = transactionFault{afterExec: inject}
			}
			installTransactionFault(t, store, fault)
			state, err := store.RecordAttempt(t.Context(), attempt)
			if state != scenario.state || (scenario.statement != "" && !errors.Is(err, failure)) || (scenario.statement == "" && err != nil) {
				t.Fatalf("approval conclusion: %s, %v; want %s", state, err, scenario.state)
			}
			proof := attempt.LocalExit("not-dispatched", "original creation was never dispatched")
			for range 2 {
				if err := store.SettleAttempt(t.Context(), attempt, proof); err != nil {
					t.Fatalf("original approval could not settle: %v", err)
				}
			}
			records, err := store.Attempts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !scenario.stored {
				if len(records) != 0 {
					t.Errorf("refused approval gained a settlement record: %+v", records)
				}
				return
			}
			if len(records) != 1 || !records[0].ExitProven() || !reflect.DeepEqual(records[0].Proof, &proof) {
				t.Fatalf("settlement changed the original attempt or proof: %+v", records)
			}
		})
	}
}

func TestAttemptSettlementCannotReplaceOriginalRequest(t *testing.T) {
	store, _, _ := testStore(t)
	attempt := protectedAttempt(t, store)
	changed := attempt
	changed.Binding = attempt.Binding.Clone()
	changed.Binding.Units[0].Configuration = value.Hash([]byte("different original request"))
	proof := changed.LocalExit("not-dispatched", "original creation was never dispatched")
	if err := store.SettleAttempt(t.Context(), changed, proof); !errors.Is(err, ErrConflict) {
		t.Fatalf("another original request settled this attempt: %v", err)
	}
	original, err := store.Attempt(t.Context(), attempt.ID)
	if err != nil || original.State != AttemptApproved || original.Proof != nil {
		t.Fatalf("refused settlement advanced the original: %+v, %v", original, err)
	}
	if err := store.SettleAttempt(t.Context(), attempt, proof); err != nil {
		t.Fatal(err)
	}
	proof.Evidence = []byte("different exit evidence")
	if err := store.SettleAttempt(t.Context(), attempt, proof); !errors.Is(err, ErrConflict) {
		t.Fatalf("settled proof was replaced: %v", err)
	}
}

func TestInterruptedAttemptApprovalKeepsOriginalSettlement(t *testing.T) {
	for _, interruption := range []string{"panic", "goexit"} {
		for _, committed := range []bool{false, true} {
			phase := "before commit"
			if committed {
				phase = "after commit"
			}
			t.Run(interruption+"/"+phase, func(t *testing.T) {
				store, _, _ := testStore(t)
				attempt := testAttempt(store)
				failure := errors.New("original attempt approval interrupted")
				var interrupted atomic.Bool
				interrupt := func(statement string) error {
					if statement == "COMMIT" && interrupted.CompareAndSwap(false, true) {
						if interruption == "panic" {
							panic(failure)
						}
						runtime.Goexit()
					}
					return nil
				}
				fault := transactionFault{beforeExec: interrupt}
				if committed {
					fault = transactionFault{afterExec: interrupt}
				}
				installTransactionFault(t, store, fault)
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					defer func() {
						recovered := recover()
						if interruption == "panic" {
							cause, ok := recovered.(error)
							if !ok || !errors.Is(cause, failure) {
								t.Errorf("original panic changed: %v", recovered)
							}
						} else if recovered != nil {
							t.Errorf("Goexit became a panic: %v", recovered)
						}
					}()
					_, err := store.RecordAttempt(t.Context(), attempt)
					t.Errorf("interrupted approval returned: %v", err)
				}()
				<-finished
				if err := store.SettleAttempt(t.Context(), attempt, attempt.LocalExit("not-dispatched", "original creation was never dispatched")); err != nil {
					t.Fatal(err)
				}
				records, err := store.Attempts(t.Context())
				if err != nil || !interrupted.Load() || len(records) > 1 || (len(records) == 1) != committed {
					t.Fatalf("interruption lost the original outcome: %+v, %v", records, err)
				}
				if committed && !records[0].ExitProven() {
					t.Fatalf("original committed approval stayed unresolved: %+v", records)
				}
			})
		}
	}
}

func TestAttemptRefusalCreatesNoProtection(t *testing.T) {
	admission := framehost.NewAdmission()
	config := testConfig(filepath.Join(t.TempDir(), "state.sqlite"), admission)
	config.MaxRequestBytes = 1
	store, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	attempt := testAttempt(store)
	state, err := store.RecordAttempt(t.Context(), attempt)
	if state != framehost.NotCommitted || !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("oversized attempt conclusion: %s, %v", state, err)
	}
	if admission.RecoveryError() != nil {
		t.Fatalf("capacity refusal fenced healthy admission: %v", admission.RecoveryError())
	}
	records, err := store.Attempts(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("refused attempt gained protection: %+v, %v", records, err)
	}
}
