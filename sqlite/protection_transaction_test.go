package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

func TestProtectionRollbackDoesNotFenceHealthyStore(t *testing.T) {
	for _, operation := range []string{"attempt approval", "use approval", "attempt settlement", "use settlement", "realm use settlement", "subordinate exit proof"} {
		for _, canceled := range []bool{false, true} {
			name := "statement failure"
			if canceled {
				name = "request cancellation"
			}
			t.Run(operation+"/"+name, func(t *testing.T) {
				store, owner, admission := testStore(t)
				attempt := protectedAttempt(t, store)
				approval := protectedUse(t, store, attempt)
				proof := attempt.LocalExit("original-exit", "original execution exited")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				failure := errors.New("test: original write interrupted")
				var apply func(context.Context) error
				var statement string
				switch operation {
				case "attempt approval":
					attempt.ID = "pending-execution"
					statement = "INSERT INTO frame_attempts"
					apply = func(ctx context.Context) error {
						state, err := store.RecordAttempt(ctx, attempt)
						if state != framehost.NotCommitted {
							t.Errorf("rolled-back approval reported %s", state)
						}
						return err
					}
				case "use approval":
					approval.ID = "pending-use"
					statement = "INSERT INTO frame_use_approvals"
					apply = func(ctx context.Context) error {
						return admission.Admit(ctx, func(ctx context.Context) error {
							state, err := store.ApproveUse(ctx, approval)
							if state != framehost.NotCommitted {
								t.Errorf("rolled-back approval reported %s", state)
							}
							return err
						})
					}
				case "attempt settlement":
					statement = "UPDATE frame_attempts"
					apply = func(ctx context.Context) error { return store.ReleaseAttempt(ctx, attempt.ID, proof) }
				case "use settlement":
					statement = "UPDATE frame_use_approvals"
					apply = func(ctx context.Context) error {
						return store.SettleUse(ctx, approval.ID, frame.ReleaseProof{CloseID: 1})
					}
				case "realm use settlement":
					store.epoch++
					statement = "UPDATE frame_use_approvals"
					apply = func(ctx context.Context) error {
						return store.ReleaseRealmUse(ctx, approval.ID, approval.Realm, attempt.ID, proof)
					}
				case "subordinate exit proof":
					statement = "INSERT INTO frame_exit_proofs"
					apply = func(ctx context.Context) error {
						return store.RecordExitProof(ctx, attempt.ID, "child", "child-exit", []byte("original exit"))
					}
				}
				var injected atomic.Bool
				var rollbacks atomic.Int32
				installTransactionFault(t, store, transactionFault{afterExec: func(sql string) error {
					if sql == "ROLLBACK" {
						rollbacks.Add(1)
					}
					if strings.HasPrefix(sql, statement) && injected.CompareAndSwap(false, true) {
						if canceled {
							cancel(failure)
							return nil
						}
						return failure
					}
					return nil
				}})
				err := apply(ctx)
				if !injected.Load() || rollbacks.Load() != 1 || !errors.Is(err, failure) || canceled && !errors.Is(err, context.Canceled) {
					t.Fatalf("original rollback or cause lost: injected=%t rollbacks=%d error=%v", injected.Load(), rollbacks.Load(), err)
				}
				if err := admission.RecoveryError(); err != nil {
					t.Errorf("confirmed rollback fenced a healthy Store: %v", err)
				}
				request := []byte("original operation terminal receipt")
				id, err := owner.Prepare(t.Context(), request)
				if err != nil {
					t.Fatalf("confirmed rollback blocked terminal receipt: %v", err)
				}
				result, err := owner.Commit(t.Context(), id, request, func(context.Context, Executor) ([]byte, error) { return []byte("finished"), nil }, nil)
				if err != nil || result.State != framehost.Committed || !result.Synchronized {
					t.Fatalf("terminal receipt could not settle: %+v %v", result, err)
				}
			})
		}
	}
}

func TestProtectionCommitUncertaintyStillFencesStore(t *testing.T) {
	for _, kind := range []string{"attempt", "use", "subordinate exit proof"} {
		t.Run(kind, func(t *testing.T) {
			store, _, admission := testStore(t)
			attempt := protectedAttempt(t, store)
			approval := protectedUse(t, store, attempt)
			failure := errors.New("test: original COMMIT acknowledgement lost")
			var injected atomic.Bool
			installTransactionFault(t, store, transactionFault{afterExec: func(statement string) error {
				if statement == "COMMIT" && injected.CompareAndSwap(false, true) {
					return failure
				}
				return nil
			}})
			var err error
			switch kind {
			case "attempt":
				err = store.ReleaseAttempt(t.Context(), attempt.ID, attempt.LocalExit("original-exit", "original execution exited"))
			case "use":
				err = store.SettleUse(t.Context(), approval.ID, frame.ReleaseProof{CloseID: 1})
			case "subordinate exit proof":
				err = store.RecordExitProof(t.Context(), attempt.ID, "child", "child-exit", []byte("original exit"))
			}
			if !injected.Load() || !errors.Is(err, failure) {
				t.Fatalf("original lost acknowledgement changed: injected=%t error=%v", injected.Load(), err)
			}
			if err := admission.Under(t.Context(), func() error { t.Error("unknown COMMIT admitted a write"); return nil }); !errors.Is(err, framehost.ErrRecoveryRequired) {
				t.Fatalf("unknown original COMMIT did not fence admission: %v", err)
			}
		})
	}
}

func TestMalformedProtectionDigestStillFencesStore(t *testing.T) {
	for _, kind := range []string{"attempt", "use"} {
		t.Run(kind, func(t *testing.T) {
			store, _, admission := testStore(t)
			attempt := protectedAttempt(t, store)
			query, id := `UPDATE frame_attempts SET request_digest=? WHERE id=?`, attempt.ID
			apply := func() error { _, err := store.RecordAttempt(t.Context(), attempt); return err }
			if kind == "use" {
				approval := protectedUse(t, store, attempt)
				query, id = `UPDATE frame_use_approvals SET request_digest=? WHERE id=?`, approval.ID
				apply = func() error {
					return admission.Admit(t.Context(), func(ctx context.Context) error { _, err := store.ApproveUse(ctx, approval); return err })
				}
			}
			if _, err := store.db.ExecContext(t.Context(), query, "invalid-digest", id); err != nil {
				t.Fatal(err)
			}
			if err := apply(); !errors.Is(err, errProtectionIntegrity) {
				t.Fatalf("malformed stored digest lost its integrity evidence: %v", err)
			}
			if err := admission.Under(t.Context(), func() error { t.Error("corrupt Store admitted a write"); return nil }); !errors.Is(err, framehost.ErrRecoveryRequired) {
				t.Fatalf("malformed protection did not fence admission: %v", err)
			}
		})
	}
}

func TestConfirmedRollbackCannotClearDatabaseCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("not a SQLite database", 8)), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	failure := db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_schema`).Scan(&name)
	if !databaseCorrupt(failure) {
		t.Fatalf("fixture did not produce real SQLite corruption evidence: %v", failure)
	}
	store, _, admission := testStore(t)
	attempt := testAttempt(store)
	ordinary := store.db.QueryRowContext(t.Context(), `SELECT missing_column FROM frame_attempts`).Scan(&name)
	if ordinary == nil || databaseCorrupt(ordinary) {
		t.Fatalf("fixture did not produce an ordinary SQL failure: %v", ordinary)
	}
	combined := errors.Join(ordinary, failure)
	var injected atomic.Bool
	installTransactionFault(t, store, transactionFault{beforeExec: func(statement string) error {
		if strings.HasPrefix(statement, "INSERT INTO frame_attempts") && injected.CompareAndSwap(false, true) {
			return combined
		}
		return nil
	}})
	state, err := store.RecordAttempt(t.Context(), attempt)
	if state != framehost.NotCommitted || !errors.Is(err, failure) {
		t.Fatalf("rollback changed the original corruption evidence: state=%s error=%v", state, err)
	}
	if !errors.Is(admission.RecoveryError(), framehost.ErrRecoveryRequired) {
		t.Fatal("database corruption left Store admission writable")
	}
}
