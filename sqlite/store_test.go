package sqlite

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func testConfig(path string, admission *frame.Admission) Config {
	return Config{Path: path, Admission: admission, Owners: []OwnerSchema{{ID: "documents", Statements: []string{`CREATE TABLE documents (id TEXT PRIMARY KEY,installation TEXT NOT NULL,release TEXT NOT NULL,active INTEGER NOT NULL)`}, References: func(ctx context.Context, tx Executor, id frame.InstallationID) ([]frame.Dependency, error) {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT installation,release FROM documents WHERE installation=? AND active=1`, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var result []frame.Dependency
		for rows.Next() {
			var dependency frame.Dependency
			if err := rows.Scan(&dependency.InstallationID, &dependency.Release); err != nil {
				return nil, err
			}
			result = append(result, dependency)
		}
		return result, rows.Err()
	}}}}
}

func testStore(t *testing.T) (*Store, *Owner, *frame.Admission) {
	t.Helper()
	admission := frame.NewAdmission()
	store, err := Open(context.Background(), testConfig(filepath.Join(t.TempDir(), "store.db"), admission))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	owner, err := store.Owner("documents")
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.RegisterUseOwner(owner); err != nil {
		t.Fatal(err)
	}
	return store, owner, admission
}

func TestSubmissionAtomicBindingAndStableConclusion(t *testing.T) {
	store, owner, admission := testStore(t)
	ctx := context.Background()
	request := []byte(`{"operation":"convert-a"}`)
	id, err := owner.Prepare(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	mutations := 0
	settlement, err := owner.Commit(ctx, id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
		mutations++
		_, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES('a','converter',?,1)`, value.Hash([]byte("release")))
		return []byte("document:a"), err
	}, nil)
	if err != nil || settlement.State != Committed || !settlement.Synchronized {
		t.Fatalf("commit: %+v %v", settlement, err)
	}
	if err := admission.Under(ctx, func() error { return admission.CheckUnused(ctx, "converter") }); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("binding was not visible: %v", err)
	}
	replayed, err := owner.Commit(ctx, id, request, func(context.Context, Executor) ([]byte, error) { mutations++; return nil, nil }, nil)
	if err != nil || replayed.State != Committed || mutations != 1 {
		t.Fatalf("repeated mutation: %+v %v count=%d", replayed, err, mutations)
	}
	if _, err := owner.Commit(ctx, id, []byte("changed"), func(context.Context, Executor) ([]byte, error) { t.Fatal("conflict executed"); return nil, nil }, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	// The business owner can release its reference without erasing its earlier
	// admission evidence. The conclusion remains Committed after completion.
	finish, err := owner.Prepare(ctx, []byte("finish"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Commit(ctx, finish, []byte("finish"), func(ctx context.Context, tx Executor) ([]byte, error) {
		_, err := tx.ExecContext(ctx, `UPDATE documents SET active=0 WHERE id='a'`)
		return nil, err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.CheckUnused(ctx, "converter"); err != nil {
		t.Fatal(err)
	}
	settlement, err = owner.Settlement(ctx, id, request)
	if err != nil || settlement.State != Committed {
		t.Fatalf("history followed current references: %+v %v", settlement, err)
	}
	var count int
	if err := store.Read(ctx, func(tx Executor) error { return tx.QueryRowContext(ctx, `SELECT count(*) FROM documents`).Scan(&count) }); err != nil || count != 1 {
		t.Fatalf("business count: %d %v", count, err)
	}
}

func TestCanceledMutationSettlesBeforeAdmissionReopens(t *testing.T) {
	store, owner, admission := testStore(t)
	request := []byte("cancel-before-commit")
	id, err := owner.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	settlement, err := owner.Commit(ctx, id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
		_, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES('cancelled','converter',?,1)`, value.Hash(nil))
		cancel()
		return nil, err
	}, nil)
	if !errors.Is(err, context.Canceled) || settlement.State != NotCommitted {
		t.Fatalf("canceled settlement: %+v %v", settlement, err)
	}
	var count int
	if err := store.Read(context.Background(), func(tx Executor) error {
		return tx.QueryRowContext(context.Background(), `SELECT count(*) FROM documents`).Scan(&count)
	}); err != nil || count != 0 {
		t.Fatalf("mutation survived rollback: %d %v", count, err)
	}
	if err := admission.CheckUnused(context.Background(), "converter"); err != nil {
		t.Fatal(err)
	}
	_, err = owner.Commit(context.Background(), id, request, func(context.Context, Executor) ([]byte, error) {
		t.Fatal("terminal NotCommitted was replayed")
		return nil, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompactionPinsContinuousPrefixAndRejectsOldReplay(t *testing.T) {
	_, owner, _ := testStore(t)
	ctx := context.Background()
	first, err := owner.Prepare(ctx, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := owner.Prepare(ctx, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Commit(ctx, second, []byte("second"), func(context.Context, Executor) ([]byte, error) { return []byte("second-record"), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Compact(ctx, second.Sequence); !errors.Is(err, ErrUnsettled) {
		t.Fatalf("compacted across prepared intent: %v", err)
	}
	_, err = owner.Commit(ctx, first, []byte("first"), func(context.Context, Executor) ([]byte, error) { return []byte("first-record"), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.RetainEvidence(ctx, first, "recovery-a"); err != nil {
		t.Fatal(err)
	}
	if err := owner.Compact(ctx, second.Sequence); !errors.Is(err, ErrUnsettled) {
		t.Fatalf("compacted retained evidence: %v", err)
	}
	if err := owner.ReleaseEvidence(ctx, first, "recovery-a"); err != nil {
		t.Fatal(err)
	}
	if err := owner.Compact(ctx, second.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Settlement(ctx, first, []byte("first")); !errors.Is(err, ErrEvidenceExpired) {
		t.Fatalf("expired query: %v", err)
	}
	if _, err := owner.Commit(ctx, second, []byte("second"), func(context.Context, Executor) ([]byte, error) {
		t.Fatal("expired operation executed")
		return nil, nil
	}, nil); !errors.Is(err, ErrEvidenceExpired) {
		t.Fatalf("expired replay: %v", err)
	}
	third, err := owner.Prepare(ctx, []byte("third"))
	if err != nil {
		t.Fatal(err)
	}
	if third.Sequence <= second.Sequence {
		t.Fatal("compacted identity was reused")
	}
}

func TestSynchronizedFailureKeepsRecoveryBarrier(t *testing.T) {
	_, owner, admission := testStore(t)
	ctx := context.Background()
	id, err := owner.Prepare(ctx, []byte("sync"))
	if err != nil {
		t.Fatal(err)
	}
	failed := errors.New("local authority unavailable")
	settlement, err := owner.Commit(ctx, id, []byte("sync"), func(context.Context, Executor) ([]byte, error) { return []byte("accepted"), nil }, func(Settlement) error { return failed })
	if !errors.Is(err, failed) || settlement.State != Committed {
		t.Fatalf("known commit lost: %+v %v", settlement, err)
	}
	if _, err := owner.Prepare(ctx, []byte("new")); !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("barrier opened: %v", err)
	}
	if err := owner.Confirm(ctx, id); err != nil {
		t.Fatal(err)
	}
	if admission.RecoveryError() == nil {
		t.Fatal("confirmation cleared unrelated recovery")
	}
	if err := admission.Recover(ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSingleAuthorityAndOwnerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(ctx, testConfig(path, frame.NewAdmission()))
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(ctx, testConfig(path, frame.NewAdmission())); err == nil {
		_ = second.Close()
		t.Fatal("second authority opened")
	}
	id, epoch := first.ID(), first.Epoch()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, testConfig(path, frame.NewAdmission()))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID() != id || second.Epoch() <= epoch {
		t.Fatal("authority identity was not preserved")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	config := testConfig(path, frame.NewAdmission())
	config.Owners[0].Statements = []string{`CREATE TABLE altered (id TEXT PRIMARY KEY)`}
	if altered, err := Open(ctx, config); err == nil {
		_ = altered.Close()
		t.Fatal("changed owner schema opened")
	}
}

func TestCurrentReferencesDoNotUseOldWALSnapshot(t *testing.T) {
	store, owner, admission := testStore(t)
	ctx := context.Background()
	var file string
	if err := store.Read(ctx, func(tx Executor) error {
		var sequence int
		var name string
		return tx.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&sequence, &name, &file)
	}); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	old, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback()
	var count int
	if err := old.QueryRow(`SELECT count(*) FROM documents`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	id, err := owner.Prepare(ctx, []byte("wal"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Commit(ctx, id, []byte("wal"), func(ctx context.Context, tx Executor) ([]byte, error) {
		_, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES('wal','converter',?,1)`, value.Hash(nil))
		return nil, err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.QueryRow(`SELECT count(*) FROM documents`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fixture did not retain old WAL view: %d %v", count, err)
	}
	if err := admission.Under(ctx, func() error { return admission.CheckUnused(ctx, "converter") }); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("current admission missed a committed reference: %v", err)
	}
}

func TestCrashSettlementUsesOriginalIntent(t *testing.T) {
	for _, phase := range []string{"before-commit", "after-commit"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crash.db")
			command := exec.Command(os.Args[0], "-test.run=^TestSQLiteCrashChild$")
			command.Env = append(os.Environ(), "FRAME_SQLITE_CHILD="+phase, "FRAME_SQLITE_PATH="+path)
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			var id SubmissionID
			if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&id); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("child identity: %v %s", err, stderr.String())
			}
			if err := command.Wait(); err == nil {
				t.Fatal("helper did not crash")
			}
			admission := frame.NewAdmission()
			store, err := Open(context.Background(), testConfig(path, admission))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			owner, _ := store.Owner("documents")
			if phase == "before-commit" {
				if admission.RecoveryError() == nil {
					t.Fatal("unsettled intent did not fence admission")
				}
				if err := owner.RecoverPrepared(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			settlement, err := owner.Settlement(context.Background(), id, []byte("crash"))
			if err != nil {
				t.Fatal(err)
			}
			want := NotCommitted
			if phase == "after-commit" {
				want = Committed
			}
			if settlement.State != want {
				t.Fatalf("settlement %s, want %s", settlement.State, want)
			}
			var count int
			if err := store.Read(context.Background(), func(tx Executor) error {
				return tx.QueryRowContext(context.Background(), `SELECT count(*) FROM documents`).Scan(&count)
			}); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != (want == Committed) {
				t.Fatalf("mutation and evidence diverged: %d %s", count, settlement.State)
			}
		})
	}
}

func TestSQLiteCrashChild(t *testing.T) {
	phase := os.Getenv("FRAME_SQLITE_CHILD")
	if phase == "" {
		return
	}
	store, err := Open(context.Background(), testConfig(os.Getenv("FRAME_SQLITE_PATH"), frame.NewAdmission()))
	if err != nil {
		panic(err)
	}
	owner, _ := store.Owner("documents")
	id, err := owner.Prepare(context.Background(), []byte("crash"))
	if err != nil {
		panic(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(id); err != nil {
		panic(err)
	}
	_, err = owner.Commit(context.Background(), id, []byte("crash"), func(ctx context.Context, tx Executor) ([]byte, error) {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO documents VALUES('crash','converter',?,1)`, value.Hash(nil))
		if err != nil {
			return nil, err
		}
		if phase == "before-commit" {
			os.Exit(23)
		}
		return []byte("document:crash"), nil
	}, nil)
	if err != nil {
		panic(err)
	}
	os.Exit(24)
}

func TestExecutionProtectionOutlivesBusinessAndEpoch(t *testing.T) {
	store, _, admission := testStore(t)
	contract, err := frame.NewContract[string]("document.converter", "1")
	if err != nil {
		t.Fatal(err)
	}
	source := frame.UnitRef{InstallationID: "converter", Release: value.Hash([]byte("release")), UnitID: "backend"}
	binding := frame.BindingSnapshot{FormatVersion: 1, Roots: []frame.ExportRef{{InstallationID: source.InstallationID, Release: source.Release, UnitID: source.UnitID, Export: "convert", Contract: contract.Ref()}}, Units: []frame.UnitBinding{{Source: source, Configuration: value.Hash(nil), Imports: []frame.ImportDeclaration{}}}}
	attempt := Attempt{ID: "attempt-1", Epoch: store.Epoch(), Source: source, Binding: binding, Realm: "runner-1", Profile: "local-process"}
	if err := store.RecordAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := admission.CheckUnused(context.Background(), "converter"); err != nil {
		t.Fatalf("idle execution became a business reference: %v", err)
	}
	dependencies, err := store.ExecutionDependencies(context.Background())
	if err != nil || len(dependencies) != 1 {
		t.Fatalf("execution protection: %v %v", dependencies, err)
	}
	attempt.Binding.Rule = "changed"
	if err := store.RecordAttempt(context.Background(), attempt); !errors.Is(err, ErrConflict) {
		t.Fatalf("widened execution: %v", err)
	}
	admission.RequireRecovery(errors.New("host restarted"))
	proof := ExitProof{AttemptID: "attempt-1", Realm: "runner-1", ExitID: "wait-1", Kind: "direct-child-wait", Evidence: []byte("original runner wait completed")}
	if err := store.ReleaseAttempt(context.Background(), "attempt-1", proof); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseAttempt(context.Background(), "attempt-1", proof); err != nil {
		t.Fatal(err)
	}
	dependencies, err = store.ExecutionDependencies(context.Background())
	if err != nil || len(dependencies) != 0 {
		t.Fatalf("proven exit retained protection: %v %v", dependencies, err)
	}
	if admission.RecoveryError() == nil {
		t.Fatal("exit proof cleared unrelated recovery")
	}
	proof.ExitID = "different"
	if err := store.ReleaseAttempt(context.Background(), "attempt-1", proof); !errors.Is(err, ErrConflict) {
		t.Fatalf("exit identity replaced: %v", err)
	}
}

func TestInitialStoreRefusesAnyForeignSchemaObject(t *testing.T) {
	for _, ddl := range []string{`CREATE VIEW outside AS SELECT 1`, `CREATE TABLE sqliteX(id)`, `CREATE TABLE sqliteY(id); CREATE TRIGGER sqliteZ AFTER INSERT ON sqliteY BEGIN SELECT 1; END`} {
		path := filepath.Join(t.TempDir(), "store.sqlite")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			store, err := Open(t.Context(), testConfig(path, frame.NewAdmission()))
			if err == nil {
				store.Close()
				t.Fatal("foreign schema accepted", ddl)
			}
			var initialized int
			if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name='frame_meta'").Scan(&initialized); err != nil || initialized != 0 {
				t.Fatalf("unexpected initialization: %d %v", initialized, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
