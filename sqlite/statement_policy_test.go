package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	modernc "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func requireUnauthorized(t *testing.T, reader Reader, statement string) {
	t.Helper()
	rows, err := reader.QueryContext(t.Context(), statement)
	if rows != nil {
		for rows.Next() {
		}
		err = errors.Join(err, rows.Err(), rows.Close())
	}
	var denied *modernc.Error
	if !errors.As(err, &denied) || denied.Code()&primaryResultMask != sqlite3.SQLITE_AUTH {
		t.Fatalf("statement escaped its authority: %s; error=%v", statement, err)
	}
}

func TestReaderCannotPromoteItsOriginalConnection(t *testing.T) {
	store, _, _ := testStore(t)
	err := store.Read(t.Context(), func(reader Reader) error {
		for _, statement := range []string{
			`PRAGMA query_only=OFF`,
			`PRAGMA query_only=''`,
			`PRAGMA foreign_keys=OFF`,
			`PRAGMA writable_schema=ON`,
			`PRAGMA journal_mode=DELETE`,
			`PRAGMA query_only=OFF; INSERT INTO documents VALUES('escaped','p','r',1) RETURNING id`,
			`INSERT INTO documents VALUES('escaped','p','r',1) RETURNING id`,
			`DELETE FROM documents RETURNING id`,
			`CREATE TABLE escaped (id TEXT)`,
			`COMMIT`,
			`ROLLBACK`,
			`SAVEPOINT escaped`,
			`ATTACH DATABASE ':memory:' AS escaped`,
		} {
			t.Run(statement, func(t *testing.T) { requireUnauthorized(t, reader, statement) })
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFaultDocumentCount(t, store, 0)
}

func TestMutationCannotReplaceConnectionOwnerControl(t *testing.T) {
	store, owner, _ := testStore(t)
	reject := errors.New("reject original mutation")
	err := owner.Update(t.Context(), func(ctx context.Context, executor Executor) error {
		if _, err := executor.ExecContext(ctx, `INSERT INTO documents VALUES('original','p','r',1)`); err != nil {
			return err
		}
		for _, statement := range []string{
			`COMMIT`, `ROLLBACK`, `SAVEPOINT escaped`, `PRAGMA foreign_keys=OFF`,
			`PRAGMA query_only=''`, `PRAGMA busy_timeout=0`, `ATTACH DATABASE ':memory:' AS escaped`,
		} {
			t.Run(statement, func(t *testing.T) { requireUnauthorized(t, executor, statement) })
		}
		return reject
	})
	if !errors.Is(err, reject) {
		t.Fatal(err)
	}
	assertFaultDocumentCount(t, store, 0)
}

func TestNestedReaderCannotOutliveItsGrantInsideTheOriginalWriter(t *testing.T) {
	database, err := OpenSchema(t.Context(), filepath.Join(t.TempDir(), "read-grant.db"), []string{`CREATE TABLE facts (value TEXT)`})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	err = RunInTx(t.Context(), database, func(writer context.Context) error {
		var escaped Executor
		if err := RunReadSnapshot(writer, database, func(reader context.Context) error {
			access := Connection(reader, database)
			escaped = access
			requireUnauthorized(t, access, `PRAGMA query_only=''`)
			requireUnauthorized(t, access, `INSERT INTO facts VALUES('denied') RETURNING value`)
			return nil
		}); err != nil {
			return err
		}
		if _, err := escaped.ExecContext(writer, `INSERT INTO facts VALUES('expired')`); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("read capability survived its own callback: %v", err)
		}
		_, err := Connection(writer, database).ExecContext(writer, `INSERT INTO facts VALUES('original writer')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.QueryRowContext(t.Context(), `SELECT count(*) FROM facts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reader changed writer facts: count=%d error=%v", count, err)
	}
}
