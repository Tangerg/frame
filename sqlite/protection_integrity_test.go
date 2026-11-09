package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func protectedAttempt(t *testing.T, store *Store) Attempt {
	t.Helper()
	source := frame.UnitRef{InstallationID: "provider", Release: value.Hash(nil), UnitID: "main"}
	binding := frame.BindingSnapshot{FormatVersion: 1, Roots: []frame.ExportRef{}, UnitRoots: []frame.UnitRef{source}, Units: []frame.UnitBinding{{Source: source, Configuration: value.Hash(nil)}}}
	attempt := Attempt{ID: "execution", Epoch: store.Epoch(), Source: source, Binding: binding, Realm: "realm", Profile: frame.LocalProcess}
	if err := store.RecordAttempt(t.Context(), attempt); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func protectedUse(t *testing.T, store *Store, attempt Attempt) frame.UseApproval {
	t.Helper()
	approval := frame.UseApproval{ID: "use", ExecutionID: attempt.ID, Realm: frame.RealmID(attempt.Realm), Scope: "scope", ScopeGeneration: 1, Epoch: store.Epoch(), Binding: attempt.Binding}
	if err := store.admission.Admit(t.Context(), func(ctx context.Context) error { return store.ApproveUse(ctx, approval) }); err != nil {
		t.Fatal(err)
	}
	return approval
}

func TestProtectionSettlementValidatesOriginalRequest(t *testing.T) {
	for _, kind := range []string{"attempt", "host use", "realm use"} {
		t.Run(kind, func(t *testing.T) {
			store, _, admission := testStore(t)
			attempt := protectedAttempt(t, store)
			proof := attempt.LocalExit("not-started", "original execution never started")
			var corrupt, stateQuery string
			var id string
			var settle func() error
			switch kind {
			case "attempt":
				id = attempt.ID
				corrupt = `UPDATE frame_attempts SET request_digest=? WHERE id=?`
				stateQuery = `SELECT state FROM frame_attempts WHERE id=?`
				settle = func() error { return store.ReleaseAttempt(t.Context(), id, proof) }
			case "host use":
				id = protectedUse(t, store, attempt).ID
				corrupt = `UPDATE frame_use_approvals SET request_digest=? WHERE id=?`
				stateQuery = `SELECT state FROM frame_use_approvals WHERE id=?`
				settle = func() error { return store.SettleUse(t.Context(), id, frame.ReleaseProof{CloseID: 1}) }
			case "realm use":
				approval := protectedUse(t, store, attempt)
				id = approval.ID
				corrupt = `UPDATE frame_use_approvals SET request_digest=? WHERE id=?`
				stateQuery = `SELECT state FROM frame_use_approvals WHERE id=?`
				store.epoch++
				settle = func() error {
					return store.ReleaseRealmUse(t.Context(), id, approval.Realm, attempt.ID, proof)
				}
			}
			if _, err := store.db.ExecContext(t.Context(), corrupt, value.Hash([]byte("corrupted request")), id); err != nil {
				t.Fatal(err)
			}
			if err := settle(); err == nil {
				t.Error("corrupted original request was allowed to release execution protection")
			}
			if admission.RecoveryError() == nil {
				t.Error("storage integrity failure did not require recovery")
			}
			var state string
			if err := store.db.QueryRowContext(t.Context(), stateQuery, id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != string(AttemptApproved) {
				t.Errorf("refused settlement changed original state to %q", state)
			}
		})
	}
}

func TestProtectionApprovalValidatesOriginalRecord(t *testing.T) {
	for _, operation := range []string{"repeat attempt", "child attempt", "repeat use", "child use"} {
		t.Run(operation, func(t *testing.T) {
			store, _, admission := testStore(t)
			attempt := protectedAttempt(t, store)
			var corrupt string
			var id string
			var approve func() error
			switch operation {
			case "repeat attempt", "child attempt":
				id = attempt.ID
				corrupt = `UPDATE frame_attempts SET request_digest=? WHERE id=?`
				if operation == "child attempt" {
					attempt.Parent, attempt.ID = attempt.ID, "child-execution"
				}
				approve = func() error { return store.RecordAttempt(t.Context(), attempt) }
			case "repeat use", "child use":
				approval := protectedUse(t, store, attempt)
				id = approval.ID
				corrupt = `UPDATE frame_use_approvals SET request_digest=? WHERE id=?`
				if operation == "child use" {
					approval.ParentID, approval.ID = approval.ID, "child-use"
				}
				approve = func() error {
					return admission.Admit(t.Context(), func(ctx context.Context) error { return store.ApproveUse(ctx, approval) })
				}
			}
			if _, err := store.db.ExecContext(t.Context(), corrupt, value.Hash([]byte("corrupted request")), id); err != nil {
				t.Fatal(err)
			}
			if err := approve(); err == nil {
				t.Error("corrupted original record admitted execution protection")
			}
			if admission.RecoveryError() == nil {
				t.Error("storage integrity failure did not require recovery")
			}
		})
	}
}

func TestStoredAttemptProofMustProveItsOriginalExit(t *testing.T) {
	for _, field := range []string{"exit identity", "kind", "evidence"} {
		t.Run(field, func(t *testing.T) {
			store, _, _ := testStore(t)
			attempt := protectedAttempt(t, store)
			proof := attempt.LocalExit("not-started", "original execution never started")
			switch field {
			case "exit identity":
				proof.ExitID = ""
			case "kind":
				proof.Kind = ""
			case "evidence":
				proof.Evidence = nil
			}
			encoded, err := json.Marshal(proof)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(t.Context(), `UPDATE frame_attempts SET state=?,proof=? WHERE id=?`, AttemptHandoff, encoded, attempt.ID); err != nil {
				t.Fatal(err)
			}
			if record, err := store.Attempt(t.Context(), attempt.ID); err == nil {
				t.Errorf("malformed exit proof restored: %+v", record)
			}
		})
	}
}

func TestStoredRealmUseProofMustMatchItsOriginalExecution(t *testing.T) {
	for _, field := range []string{"execution", "realm", "evidence"} {
		t.Run(field, func(t *testing.T) {
			store, _, _ := testStore(t)
			attempt := protectedAttempt(t, store)
			approval := protectedUse(t, store, attempt)
			proof := attempt.LocalExit("not-started", "original execution never started")
			switch field {
			case "execution":
				proof.AttemptID = "foreign-execution"
			case "realm":
				proof.Realm = "foreign-realm"
			case "evidence":
				proof.Evidence = nil
			}
			encoded, err := json.Marshal(UseProof{Realm: &proof})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(t.Context(), `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, useReleased, encoded, approval.ID); err != nil {
				t.Fatal(err)
			}
			if record, err := store.Use(t.Context(), approval.ID); err == nil {
				t.Errorf("foreign or missing execution proof restored: %+v", record)
			}
		})
	}
}

func TestAttemptWithoutProvenStateDoesNotClaimExit(t *testing.T) {
	if (AttemptRecord{}).ExitProven() {
		t.Fatal("zero-value attempt claimed that execution has exited")
	}
}

func TestStoredAttemptCannotDelegateToItself(t *testing.T) {
	store, _, _ := testStore(t)
	attempt := protectedAttempt(t, store)
	attempt.Parent = attempt.ID
	encoded, err := json.Marshal(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE frame_attempts SET request=?,request_digest=? WHERE id=?`, encoded, value.Hash(encoded), attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Attempt(t.Context(), attempt.ID); err == nil {
		t.Fatal("stored attempt accepted a self-parent delegation")
	}
}
