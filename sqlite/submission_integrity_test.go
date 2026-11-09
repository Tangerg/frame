package sqlite

import (
	"context"
	"errors"
	"testing"
)

func TestSubmissionSchemaRejectsImpossibleStates(t *testing.T) {
	store, owner, _ := testStore(t)
	id, err := owner.Prepare(t.Context(), []byte("original intent"))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE frame_submissions SET synchronized=1 WHERE sequence=?`,
		`UPDATE frame_submissions SET evidence=x'01' WHERE sequence=?`,
		`UPDATE frame_submissions SET state='invalid' WHERE sequence=?`,
	} {
		if _, err := store.db.ExecContext(t.Context(), query, id.Sequence); err == nil {
			t.Fatalf("schema admitted an impossible submission: %s", query)
		}
	}
}

func TestCorruptSubmissionFencesEverySettlementBoundary(t *testing.T) {
	for _, corruption := range []string{
		`UPDATE frame_submissions SET evidence=x'01'`,
		`UPDATE frame_submissions SET state='not_committed', evidence=x'01'`,
		`UPDATE frame_submissions SET state='invalid'`,
		`UPDATE frame_submissions SET request=x'01'`,
		`UPDATE frame_submissions SET digest='invalid'`,
	} {
		for _, operation := range []string{"commit", "settlement", "pending", "recover", "confirm", "compact"} {
			t.Run(operation+"/"+corruption, func(t *testing.T) {
				store, owner, admission := testStore(t)
				request := []byte("original intent")
				id, err := owner.Prepare(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				for _, query := range []string{`PRAGMA ignore_check_constraints=ON`, corruption, `PRAGMA ignore_check_constraints=OFF`} {
					if _, err := store.db.ExecContext(t.Context(), query); err != nil {
						t.Fatal(err)
					}
				}
				switch operation {
				case "commit":
					_, err = owner.Commit(t.Context(), id, request, func(context.Context, Executor) ([]byte, error) {
						t.Fatal("corrupt intent ran business mutation")
						return nil, nil
					}, func(Settlement) error { t.Fatal("corrupt intent synchronized"); return nil })
				case "settlement":
					_, err = owner.Settlement(t.Context(), id, request)
				case "pending":
					_, err = owner.Pending(t.Context())
				case "recover":
					err = owner.RecoverPrepared(t.Context())
				case "confirm":
					err = owner.Confirm(t.Context(), id)
				case "compact":
					err = owner.Compact(t.Context(), id.Sequence)
				}
				if !errors.Is(err, ErrIndeterminate) || admission.RecoveryError() == nil {
					t.Fatalf("corrupt submission admitted at %s: error=%v recovery=%v", operation, err, admission.RecoveryError())
				}
			})
		}
	}
}
