package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	framehost "github.com/Tangerg/frame/host"
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
		`DELETE FROM frame_submissions`,
	} {
		for _, operation := range []string{"commit", "settlement", "pending", "recover", "confirm", "retain", "compact", "compact-settled"} {
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
				case "retain":
					err = owner.RetainEvidence(t.Context(), id, "business-recovery")
				case "compact":
					err = owner.Compact(t.Context(), id.Sequence)
				case "compact-settled":
					err = owner.CompactSettled(t.Context())
				}
				if !errors.Is(err, ErrIndeterminate) || admission.RecoveryError() == nil {
					t.Fatalf("corrupt submission admitted at %s: error=%v recovery=%v", operation, err, admission.RecoveryError())
				}
			})
		}
	}
}

func TestMissingIssuedEvidenceFencesTheWholeRetainedInterval(t *testing.T) {
	for _, sequence := range []uint64{1, 2, 3} {
		for _, operation := range []string{"prepare", "pending", "recover", "compact-settled", "reopen"} {
			t.Run(operation+"/"+strconv.FormatUint(sequence, 10), func(t *testing.T) {
				admission := framehost.NewAdmission()
				config := testConfig(filepath.Join(t.TempDir(), "evidence.db"), admission)
				store, err := Open(t.Context(), config)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				owner, err := store.Owner("documents")
				if err != nil {
					t.Fatal(err)
				}
				for range 3 {
					if _, err := owner.Prepare(t.Context(), []byte("original intent")); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.db.ExecContext(t.Context(), `DELETE FROM frame_submissions WHERE owner_id=? AND sequence=?`, owner.id, sequence); err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "prepare":
					_, err = owner.Prepare(t.Context(), []byte("later intent"))
				case "pending":
					_, err = owner.Pending(t.Context())
				case "recover":
					err = owner.RecoverPrepared(t.Context())
				case "compact-settled":
					err = owner.CompactSettled(t.Context())
				case "reopen":
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					admission = framehost.NewAdmission()
					config.Admission = admission
					var reopened Store
					reopened, err = Open(t.Context(), config)
					defer reopened.Close()
				}
				if !errors.Is(err, ErrIndeterminate) || admission.RecoveryError() == nil {
					t.Fatalf("lost issued evidence admitted at %s: error=%v recovery=%v", operation, err, admission.RecoveryError())
				}
			})
		}
	}
}

func TestUnissuedAndCompactedIdentitiesDoNotFenceAdmission(t *testing.T) {
	_, owner, admission := testStore(t)
	request := []byte("original intent")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	unissued := id
	unissued.Sequence++
	if _, err := owner.Settlement(t.Context(), unissued, request); !errors.Is(err, ErrUnknownSubmission) {
		t.Fatalf("unissued identity: %v", err)
	}
	if err := owner.RecoverPrepared(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := owner.Confirm(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := owner.CompactSettled(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Settlement(t.Context(), id, request); !errors.Is(err, ErrEvidenceExpired) {
		t.Fatalf("compacted identity: %v", err)
	}
	if admission.RecoveryError() != nil {
		t.Fatalf("ordinary identity error fenced the owner: %v", admission.RecoveryError())
	}
	if _, err := owner.Prepare(t.Context(), []byte("next intent")); err != nil {
		t.Fatalf("compacted interval prevented issuing new evidence: %v", err)
	}
}
