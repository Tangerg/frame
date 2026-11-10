package sqlite

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
)

func TestProtectionQueriesAndReopenRejectUnprovenState(t *testing.T) {
	for _, test := range []struct {
		name         string
		attemptState AttemptState
		useState     useState
		withProof    bool
		foreignRealm bool
	}{
		{name: "approved attempt with proof", attemptState: AttemptApproved, withProof: true},
		{name: "handoff without proof", attemptState: AttemptHandoff},
		{name: "released attempt without proof", attemptState: AttemptReleased},
		{name: "handoff with foreign proof", attemptState: AttemptHandoff, withProof: true, foreignRealm: true},
		{name: "released attempt with foreign proof", attemptState: AttemptReleased, withProof: true, foreignRealm: true},
		{name: "approved use with proof", useState: useApproved, withProof: true},
		{name: "released use without proof", useState: useReleased},
		{name: "released use with foreign proof", useState: useReleased, withProof: true, foreignRealm: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			store, err := Open(t.Context(), testConfig(path, frame.NewAdmission()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			attempt := protectedAttempt(t, store)
			proof := attempt.LocalExit("not-started", "original execution never started")
			if test.foreignRealm {
				proof.Realm = "foreign-realm"
			}
			var encoded []byte
			if test.useState != "" {
				approval := protectedUse(t, store, attempt)
				if err := store.ReleaseAttempt(t.Context(), attempt.ID, attempt.LocalExit("not-started", "original execution never started")); err != nil {
					t.Fatal(err)
				}
				if test.withProof {
					encoded, err = json.Marshal(UseProof{Realm: &proof})
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.db.ExecContext(t.Context(), `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, test.useState, encoded, approval.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Use(t.Context(), approval.ID); err == nil {
					t.Fatal("individual use lookup accepted invalid evidence")
				}
			} else {
				if test.withProof {
					encoded, err = json.Marshal(proof)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.db.ExecContext(t.Context(), `UPDATE frame_attempts SET state=?,proof=? WHERE id=?`, test.attemptState, encoded, attempt.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Attempt(t.Context(), attempt.ID); err == nil {
					t.Fatal("individual attempt lookup accepted invalid evidence")
				}
				if pending, err := store.PendingAttempts(t.Context()); err == nil {
					t.Fatalf("pending query accepted invalid evidence: %+v", pending)
				}
			}
			if dependencies, err := store.ExecutionDependencies(t.Context()); err == nil {
				t.Fatalf("dependency query accepted invalid evidence: %+v", dependencies)
			}
			epoch := store.Epoch()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(t.Context(), testConfig(path, frame.NewAdmission()))
			if reopened != nil {
				_ = reopened.Close()
			}
			if err == nil {
				t.Fatal("reopen accepted invalid protection evidence")
			}
			database, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			var persistedEpoch uint64
			if err := database.QueryRowContext(t.Context(), `SELECT epoch FROM frame_meta`).Scan(&persistedEpoch); err != nil || persistedEpoch != epoch {
				t.Fatalf("rejected recovery advanced epoch: got=%d want=%d err=%v", persistedEpoch, epoch, err)
			}
		})
	}
}

func TestReopenDerivesProtectionRecoveryFromValidatedRecords(t *testing.T) {
	for _, test := range []struct {
		name             string
		state            AttemptState
		useState         useState
		dependencies     int
		pending          int
		recoveryRequired bool
	}{
		{name: "approved attempt", state: AttemptApproved, dependencies: 1, pending: 1, recoveryRequired: true},
		{name: "pending acknowledgement", state: AttemptHandoff, pending: 1, recoveryRequired: true},
		{name: "released attempt", state: AttemptReleased},
		{name: "approved use", state: AttemptReleased, useState: useApproved, dependencies: 1, recoveryRequired: true},
		{name: "released use", state: AttemptReleased, useState: useReleased},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			store, err := Open(t.Context(), testConfig(path, frame.NewAdmission()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			attempt := protectedAttempt(t, store)
			if test.useState != "" {
				approval := protectedUse(t, store, attempt)
				if test.useState == useReleased {
					if err := store.SettleUse(t.Context(), approval.ID, frame.ReleaseProof{CloseID: 1}); err != nil {
						t.Fatal(err)
					}
				}
			}
			proof := attempt.LocalExit("not-started", "original execution never started")
			switch test.state {
			case AttemptHandoff:
				err = store.AcceptExit(t.Context(), attempt.ID, proof)
			case AttemptReleased:
				err = store.ReleaseAttempt(t.Context(), attempt.ID, proof)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			admission := frame.NewAdmission()
			reopened, err := Open(t.Context(), testConfig(path, admission))
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if got := admission.RecoveryError() != nil; got != test.recoveryRequired {
				t.Fatalf("recovery required=%v want=%v", got, test.recoveryRequired)
			}
			if dependencies, err := reopened.ExecutionDependencies(t.Context()); err != nil || len(dependencies) != test.dependencies {
				t.Fatalf("protection=%+v want count=%d err=%v", dependencies, test.dependencies, err)
			}
			if pending, err := reopened.PendingAttempts(t.Context()); err != nil || len(pending) != test.pending {
				t.Fatalf("pending=%+v want count=%d err=%v", pending, test.pending, err)
			}
		})
	}
}
