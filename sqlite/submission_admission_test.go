package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
)

func TestJointSubmissionRequiresTheOriginalLiveAdmission(t *testing.T) {
	store, owner, admission := testStore(t)
	request := []byte("joint-write")
	if _, err := owner.PrepareUnder(t.Context(), request); err == nil {
		t.Fatal("unadmitted caller prepared a submission")
	}
	foreign := frame.NewAdmission()
	if err := foreign.Admit(t.Context(), func(ctx context.Context) error {
		_, err := owner.PrepareUnder(ctx, request)
		return err
	}); err == nil {
		t.Fatal("foreign authority prepared a submission")
	}
	var escaped context.Context
	var id SubmissionID
	err := admission.Admit(t.Context(), func(ctx context.Context) error {
		escaped = ctx
		var err error
		id, err = owner.PrepareUnder(ctx, request)
		if err != nil {
			return err
		}
		settlement, err := owner.CommitUnder(ctx, id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
			_, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES('joint','converter',?,1)`, integrity.Hash([]byte("release")))
			return []byte("joint"), err
		}, nil)
		if err == nil && (settlement.State != Committed || !settlement.Synchronized) {
			t.Fatalf("joint settlement = %+v", settlement)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.PrepareUnder(escaped, []byte("late")); err == nil {
		t.Fatal("expired admission context created a submission")
	}
	if _, err := owner.CommitUnder(escaped, id, request, func(context.Context, Executor) ([]byte, error) {
		t.Fatal("expired admission ran a mutation")
		return nil, nil
	}, nil); err == nil {
		t.Fatal("expired context reentered the original transaction")
	}
	var count int
	if err := store.Read(t.Context(), func(tx Executor) error {
		return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM documents WHERE id='joint'`).Scan(&count)
	}); err != nil || count != 1 {
		t.Fatalf("joint write count = %d, %v", count, err)
	}
}

func TestSubmissionCapacityRetainsUnsettledPrefixAndReclaimsOnlyConfirmedEvidence(t *testing.T) {
	admission := frame.NewAdmission()
	config := testConfig(filepath.Join(t.TempDir(), "bounded.db"), admission)
	config.MaxSubmissionsPerOwner = 2
	store, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, err := store.Owner("documents")
	if err != nil {
		t.Fatal(err)
	}
	first, err := owner.Prepare(t.Context(), []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := owner.Prepare(t.Context(), []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Prepare(t.Context(), []byte("third")); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("unsettled capacity = %v", err)
	}
	if err := owner.CompactSettled(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Prepare(t.Context(), []byte("third")); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("compaction guessed an unsettled outcome: %v", err)
	}
	if _, err := owner.Commit(t.Context(), first, []byte("first"), func(context.Context, Executor) ([]byte, error) {
		return []byte("original evidence"), nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := owner.CompactSettled(t.Context()); err != nil {
		t.Fatal(err)
	}
	third, err := owner.Prepare(t.Context(), []byte("third"))
	if err != nil || third.Sequence != second.Sequence+1 {
		t.Fatalf("reclaimed capacity = %+v, %v", third, err)
	}
	if _, err := owner.Settlement(t.Context(), first, []byte("first")); !errors.Is(err, ErrEvidenceExpired) {
		t.Fatalf("compacted sequence was reusable: %v", err)
	}
	if _, err := owner.Commit(t.Context(), first, []byte("first"), func(context.Context, Executor) ([]byte, error) {
		t.Fatal("expired original intent was executed again")
		return nil, nil
	}, nil); !errors.Is(err, ErrEvidenceExpired) {
		t.Fatalf("expired commit = %v", err)
	}
}
