package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/sqlite"
)

func TestTransactionsKeepIndependentDatabaseOwners(t *testing.T) {
	schema := []string{`CREATE TABLE IF NOT EXISTS facts (value TEXT NOT NULL)`}
	first, err := sqlite.OpenSchema(t.Context(), filepath.Join(t.TempDir(), "first.db"), schema)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := sqlite.OpenSchema(t.Context(), filepath.Join(t.TempDir(), "second.db"), schema)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	failure := errors.New("first owner refused")
	err = first.RunInTx(t.Context(), func(ctx context.Context) error {
		if _, err := first.Executor(ctx).ExecContext(ctx, `INSERT INTO facts VALUES ('first')`); err != nil {
			return err
		}
		if err := second.RunInTx(ctx, func(ctx context.Context) error {
			_, err := second.Executor(ctx).ExecContext(ctx, `INSERT INTO facts VALUES ('second')`)
			return err
		}); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	var firstCount, secondCount int
	if err := databaseQuery(first, t.Context(), `SELECT count(*) FROM facts`).Scan(&firstCount); err != nil {
		t.Fatal(err)
	}
	if err := databaseQuery(second, t.Context(), `SELECT count(*) FROM facts`).Scan(&secondCount); err != nil {
		t.Fatal(err)
	}
	if firstCount != 0 || secondCount != 1 {
		t.Fatalf("database owners were conflated: first=%d second=%d", firstCount, secondCount)
	}
}

func TestIndependentTransactionRefusesItsOwnActiveDatabase(t *testing.T) {
	db, err := sqlite.OpenSchema(t.Context(), filepath.Join(t.TempDir(), "private.db"), []string{`CREATE TABLE IF NOT EXISTS facts (value TEXT NOT NULL)`})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RunInTx(t.Context(), func(ctx context.Context) error {
		if err := db.RunIndependentTx(ctx, func(context.Context) error { t.Fatal("independent transaction entered its active owner"); return nil }); err == nil {
			t.Fatal("recursive independent transaction was accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReadSnapshotJoinsItsOwnerButCannotPromoteToAWriteSet(t *testing.T) {
	db, err := sqlite.OpenSchema(t.Context(), filepath.Join(t.TempDir(), "snapshot.db"), []string{`CREATE TABLE IF NOT EXISTS facts (value TEXT NOT NULL)`})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RunInTx(t.Context(), func(ctx context.Context) error {
		if _, err := db.Executor(ctx).ExecContext(ctx, `INSERT INTO facts VALUES ('owned')`); err != nil {
			return err
		}
		return db.RunReadSnapshot(ctx, func(ctx context.Context) error {
			if _, err := db.Executor(ctx).ExecContext(ctx, `DELETE FROM facts`); err == nil {
				t.Fatal("nested read scope modified the enclosing writer")
			}
			var value string
			if err := db.Executor(ctx).QueryRowContext(ctx, `SELECT value FROM facts`).Scan(&value); err != nil {
				return err
			}
			if value != "owned" {
				t.Fatalf("nested snapshot lost its uncommitted owner value: %q", value)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RunReadSnapshot(t.Context(), func(ctx context.Context) error {
		return db.RunReadSnapshot(ctx, func(ctx context.Context) error {
			if _, err := db.Executor(ctx).ExecContext(ctx, `INSERT INTO facts VALUES ('forbidden')`); err == nil {
				t.Fatal("SQLite executed a write in a read-only snapshot")
			}
			if err := db.RunInTx(ctx, func(context.Context) error { t.Fatal("read snapshot entered a mutation"); return nil }); err == nil {
				t.Fatal("read snapshot accepted a write-set")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}
