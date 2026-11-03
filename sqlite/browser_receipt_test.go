package sqlite

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestRealmReceiptReservesItsCompletionBeforeCapacityIsExhausted(t *testing.T) {
	store, err := Open(t.Context(), Config{Path: filepath.Join(t.TempDir(), "history.db"), Admission: frame.NewAdmission(), MaxRealmHistoryBytes: 258})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first, err := store.ReserveRealmReceipt(t.Context(), "first", value.Hash([]byte("first")), 40)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ReserveRealmReceipt(t.Context(), "other", value.Hash([]byte("other")), 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveRealmReceipt(t.Context(), "first", value.Hash([]byte("another request")), 40); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending identity admitted another request: %v", err)
	}
	if _, err := store.ReserveRealmReceipt(t.Context(), "extra", value.Hash([]byte("extra")), 1); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("unreserved operation exceeded capacity: %v", err)
	}
	if _, err := store.RealmReceipt(t.Context(), "first"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reservation became a completed receipt: %v", err)
	}
	if err := first.Record(t.Context(), bytes.Repeat([]byte("x"), 41)); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("receipt exceeded its admission envelope: %v", err)
	}
	original := bytes.Repeat([]byte("x"), 40)
	if err := first.Record(t.Context(), original); err != nil {
		t.Fatalf("another reservation prevented accepted completion: %v", err)
	}
	replayed, err := store.ReserveRealmReceipt(t.Context(), "first", value.Hash([]byte("first")), 40)
	if err != nil {
		t.Fatalf("replay requested new capacity: %v", err)
	}
	if err := replayed.Record(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if err := replayed.Record(t.Context(), []byte("changed")); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay changed the original result: %v", err)
	}
	if _, err := store.ReserveRealmReceipt(t.Context(), "first", value.Hash([]byte("first")), 39); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay changed the original reservation: %v", err)
	}
	if err := other.Record(t.Context(), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveRealmReceipt(t.Context(), "extra", value.Hash([]byte("extra")), 7); err != nil {
		t.Fatalf("completion did not return unused envelope bytes: %v", err)
	}
	if _, err := store.ReserveRealmReceipt(t.Context(), "fourth", value.Hash([]byte("fourth")), 1); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("completed receipts lost their charge: %v", err)
	}
	data, err := store.RealmReceipt(t.Context(), "first")
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("full history prevented observation: %q, %v", data, err)
	}
}

func TestRealmCredentialsShareTheHistoryBudgetAndSurviveEpochRotation(t *testing.T) {
	credential := RealmCredential{ID: "realm", Epoch: "1", OperationID: "open", ResumeVerifier: value.Hash([]byte("resume secret"))}
	credentialBytes := int64(len(credential.ID) + len(credential.Epoch) + len(credential.OperationID) + len(credential.ResumeVerifier.String()))
	config := Config{Path: filepath.Join(t.TempDir(), "history.db"), Admission: frame.NewAdmission(), MaxRealmHistoryBytes: credentialBytes + int64(len(value.Hash([]byte("reply")).String())) + 8}
	store, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.RegisterRealm(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	reservation, err := store.ReserveRealmReceipt(t.Context(), "reply", value.Hash([]byte("reply")), 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Record(t.Context(), []byte("yes")); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterRealm(t.Context(), credential); err != nil {
		t.Fatalf("lost OpenRealm acknowledgement could not resume at capacity: %v", err)
	}
	second := credential
	second.ID, second.OperationID = "other", "another"
	if err := store.RegisterRealm(t.Context(), second); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("new credential escaped the shared budget: %v", err)
	}
	if _, err := store.RealmOperation(t.Context(), second.OperationID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected credential was retained: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	config.Admission = frame.NewAdmission()
	store, err = Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RealmReceipt(t.Context(), "reply"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("previous live authority retained a replay capability: %v", err)
	}
	restored, err := store.RealmCredential(t.Context(), credential.ID, credential.Epoch)
	if err != nil || restored != credential {
		t.Fatalf("epoch rotation lost original cleanup authentication: %+v, %v", restored, err)
	}
	if _, err := store.ReserveRealmReceipt(t.Context(), "reply", value.Hash([]byte("reply")), 3); err != nil {
		t.Fatalf("epoch rotation did not reclaim live-authority receipts: %v", err)
	}
	if err := store.RegisterRealm(t.Context(), second); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("old credentials stopped counting after rotation: %v", err)
	}
}

func TestRealmHistoryRejectsInvalidLimitsAndUnadmittedIdentities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	if _, err := Open(t.Context(), Config{Path: path, Admission: frame.NewAdmission(), MaxRealmHistoryBytes: -1}); err == nil {
		t.Fatal("negative history budget was admitted")
	}
	store, err := Open(t.Context(), Config{Path: path, Admission: frame.NewAdmission(), MaxRealmHistoryBytes: 258})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, size := range []int64{0, -1} {
		if _, err := store.ReserveRealmReceipt(t.Context(), "invalid", value.Hash([]byte("invalid")), size); !errors.Is(err, frame.ErrInvalid) {
			t.Fatalf("invalid reservation %d: %v", size, err)
		}
	}
	if _, err := store.ReserveRealmReceipt(t.Context(), "oversized", value.Hash([]byte("oversized")), 1<<63-1); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("reservation arithmetic overflowed: %v", err)
	}
	if err := new(ReceiptReservation).Record(t.Context(), []byte("unadmitted")); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("unadmitted receipt was recorded: %v", err)
	}
}
