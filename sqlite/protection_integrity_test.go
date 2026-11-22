package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func testAttempt(store Store) Attempt {
	source := framehost.UnitRef{InstallationID: "provider", Release: value.Hash(nil), UnitID: "main"}
	binding := framehost.BindingSnapshot{Roots: []framehost.ExportRef{}, UnitRoots: []framehost.UnitRef{source}, Units: []framehost.UnitBinding{{Source: source, Configuration: value.Hash(nil)}}}
	return Attempt{ID: "execution", Epoch: store.Epoch(), Source: source, Binding: binding, Realm: "realm", Profile: framehost.LocalProcess, Scope: "scope", ScopeGeneration: 1, Generation: 1}
}

func protectedAttempt(t *testing.T, store Store) Attempt {
	t.Helper()
	attempt := testAttempt(store)
	if state, err := store.RecordAttempt(t.Context(), attempt); state != framehost.Committed || err != nil {
		t.Fatalf("original attempt approval: %s, %v", state, err)
	}
	return attempt
}

func protectedUse(t *testing.T, store Store, attempt Attempt) framehost.UseApproval {
	t.Helper()
	approval := framehost.UseApproval{ID: "use", ExecutionID: attempt.ID, Realm: framehost.RealmID(attempt.Realm), Scope: "scope", ScopeGeneration: 1, Epoch: store.Epoch(), Binding: attempt.Binding}
	if err := store.admission.Admit(t.Context(), func(ctx context.Context) error { _, err := store.ApproveUse(ctx, approval); return err }); err != nil {
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
	for _, operation := range []string{"repeat attempt", "repeat use", "child use"} {
		t.Run(operation, func(t *testing.T) {
			store, _, admission := testStore(t)
			attempt := protectedAttempt(t, store)
			var corrupt string
			var id string
			var approve func() error
			switch operation {
			case "repeat attempt":
				id = attempt.ID
				corrupt = `UPDATE frame_attempts SET request_digest=? WHERE id=?`
				approve = func() error { _, err := store.RecordAttempt(t.Context(), attempt); return err }
			case "repeat use", "child use":
				approval := protectedUse(t, store, attempt)
				id = approval.ID
				corrupt = `UPDATE frame_use_approvals SET request_digest=? WHERE id=?`
				if operation == "child use" {
					approval.ParentID, approval.ID = approval.ID, "child-use"
				}
				approve = func() error {
					return admission.Admit(t.Context(), func(ctx context.Context) error { _, err := store.ApproveUse(ctx, approval); return err })
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

func TestStoredAttemptRequiresCompleteScope(t *testing.T) {
	store, _, _ := testStore(t)
	attempt := protectedAttempt(t, store)
	attempt.ScopeGeneration = 0
	encoded, err := json.Marshal(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE frame_attempts SET request=?,request_digest=? WHERE id=?`, encoded, value.Hash(encoded), attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Attempt(t.Context(), attempt.ID); err == nil {
		t.Fatal("stored attempt accepted a scope without its generations")
	}
}
