package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

func submissionExitStore(t *testing.T) (sqlite.Store, sqlite.Owner, framehost.Admission) {
	t.Helper()
	admission := framehost.NewAdmission()
	store, err := sqlite.Open(t.Context(), sqlite.Config{
		Path: filepath.Join(t.TempDir(), "submission-exit.db"), Admission: admission,
		Owners: []sqlite.OwnerSchema{{
			ID: "tasks", Statements: []string{`CREATE TABLE task_records(id TEXT PRIMARY KEY)`},
			References: func(context.Context, sqlite.Reader, framehost.InstallationID) ([]framehost.Dependency, error) {
				return nil, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	owner, err := store.Owner("tasks")
	if err != nil {
		t.Fatal(err)
	}
	return store, owner, admission
}

func awaitSubmissionGoexit(t *testing.T, commit func()) {
	t.Helper()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		commit()
		t.Error("submission callback's Goexit was swallowed")
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("submission did not join its interrupted transaction")
	}
}

func TestSubmissionMutationGoexitSettlesTheOriginalRejection(t *testing.T) {
	store, owner, admission := submissionExitStore(t)
	request := []byte("original task")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	mutations := 0
	awaitSubmissionGoexit(t, func() {
		_, _ = owner.Commit(t.Context(), id, request, func(ctx context.Context, tx sqlite.Executor) ([]byte, error) {
			mutations++
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_records VALUES('original')`); err != nil {
				t.Error(err)
			}
			runtime.Goexit()
			return nil, nil
		}, nil)
	})
	settled, err := owner.Settlement(t.Context(), id, request)
	if err != nil || settled.State != framehost.CommitRejected || !settled.Synchronized {
		t.Errorf("abandoned mutation did not settle its original rejection: %+v %v", settled, err)
	}
	var records int
	if err := store.Read(t.Context(), func(tx sqlite.Reader) error {
		return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM task_records`).Scan(&records)
	}); err != nil || records != 0 {
		t.Fatalf("interrupted original write-set did not roll back: count=%d error=%v", records, err)
	}
	if err := admission.RecoveryError(); err != nil {
		t.Fatalf("confirmed rollback retained unknown commit debt: %v", err)
	}
	settled, err = owner.Commit(t.Context(), id, request, func(context.Context, sqlite.Executor) ([]byte, error) {
		mutations++
		return []byte("replacement evidence"), nil
	}, nil)
	if err != nil || settled.State != framehost.CommitRejected || mutations != 1 {
		t.Fatalf("original intent authorized another mutation: calls=%d settlement=%+v error=%v", mutations, settled, err)
	}
}

func TestSubmissionSynchronizationGoexitRetainsRecoveryBarrier(t *testing.T) {
	_, owner, admission := submissionExitStore(t)
	request := []byte("committed task")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	awaitSubmissionGoexit(t, func() {
		_, _ = owner.Commit(t.Context(), id, request, func(ctx context.Context, tx sqlite.Executor) ([]byte, error) {
			_, err := tx.ExecContext(ctx, `INSERT INTO task_records VALUES('committed')`)
			return []byte("committed"), err
		}, func(sqlite.Settlement) error {
			runtime.Goexit()
			return nil
		})
	})
	settled, err := owner.Settlement(t.Context(), id, request)
	if err != nil || settled.State != framehost.CommitSucceeded || settled.Synchronized {
		t.Fatalf("synchronization interruption changed the original commit: %+v %v", settled, err)
	}
	if err := admission.RecoveryError(); !errors.Is(err, framehost.ErrRecoveryRequired) {
		t.Errorf("unsynchronized commit left admission open: %v", err)
	}
	if _, err := owner.Prepare(t.Context(), []byte("new task")); !errors.Is(err, framehost.ErrRecoveryRequired) {
		t.Fatalf("new work crossed the unsynchronized original commit: %v", err)
	}
}
