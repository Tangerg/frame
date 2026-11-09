package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/Tangerg/frame/sqlite"
)

func TestReadSnapshotClosesItsCursorsBeforeRestoringWriter(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	database, _ := readScopeDatabase(t)
	var expiredWriter context.Context
	err := sqlite.RunInTx(t.Context(), database, func(writer context.Context) error {
		expiredWriter = writer
		var rows *sql.Rows
		var row *sqlite.Row
		var expired context.Context
		if err := sqlite.RunReadSnapshot(writer, database, func(reader context.Context) error {
			expired = reader
			access := sqlite.Connection(reader, database)
			var err error
			rows, err = access.QueryContext(reader, `SELECT 'first' UNION ALL SELECT 'second'`)
			if err != nil {
				return err
			}
			row = access.QueryRowContext(reader, `SELECT 'unscanned'`)
			return row.Err()
		}); err != nil {
			return err
		}
		if rows.Next() {
			t.Fatal("cursor continued after its read callback returned")
		}
		var value string
		if err := row.Scan(&value); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("single-row cursor survived its callback: value=%q error=%v", value, err)
		}
		if err := row.Err(); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("expired row concealed its original callback lifetime: %v", err)
		}
		for _, enter := range []func(context.Context, *sqlite.Database, func(context.Context) error) error{sqlite.RunReadSnapshot, sqlite.RunInTx} {
			entered := false
			err := enter(expired, database, func(context.Context) error { entered = true; return nil })
			if entered || !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("expired context issued another callback: entered=%t error=%v", entered, err)
			}
		}
		_, err := sqlite.Connection(writer, database).ExecContext(writer, `INSERT INTO facts VALUES('original writer')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.RunInTx(expiredWriter, database, func(context.Context) error {
		t.Fatal("expired writer context issued another callback")
		return nil
	}); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("expired writer context: %v", err)
	}
	var value string
	if err := databaseQuery(database, t.Context(), `SELECT value FROM facts`).Scan(&value); err != nil || value != "original writer" {
		t.Fatalf("cursor cleanup lost its original writer: value=%q error=%v", value, err)
	}
}

func TestRowKeepsSingleRowScanSemantics(t *testing.T) {
	database, _ := readScopeDatabase(t)
	err := database.Read(t.Context(), func(access sqlite.Reader) error {
		t.Run("first result", func(t *testing.T) {
			row := access.QueryRowContext(t.Context(), `SELECT 'first' UNION ALL SELECT 'second'`)
			if err := row.Err(); err != nil {
				t.Fatal(err)
			}
			var value string
			if err := row.Scan(&value); err != nil || value != "first" {
				t.Fatalf("single-row result: value=%q error=%v", value, err)
			}
		})
		t.Run("no result", func(t *testing.T) {
			row := access.QueryRowContext(t.Context(), `SELECT value FROM facts`)
			if err := row.Err(); err != nil {
				t.Fatal(err)
			}
			var value string
			if err := row.Scan(&value); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("empty result: %v", err)
			}
		})
		t.Run("query error", func(t *testing.T) {
			row := access.QueryRowContext(t.Context(), `SELECT missing FROM facts`)
			original := row.Err()
			var value string
			if original == nil || !errors.Is(row.Scan(&value), original) {
				t.Fatalf("query error was not preserved: %v", original)
			}
		})
		t.Run("conversion error", func(t *testing.T) {
			var value int
			if err := access.QueryRowContext(t.Context(), `SELECT 'text'`).Scan(&value); err == nil {
				t.Fatal("invalid conversion was accepted")
			}
			if err := access.QueryRowContext(t.Context(), `SELECT 7`).Scan(&value); err != nil || value != 7 {
				t.Fatalf("conversion error retained the only connection: value=%d error=%v", value, err)
			}
		})
		t.Run("borrowed bytes", func(t *testing.T) {
			var borrowed sql.RawBytes
			if err := access.QueryRowContext(t.Context(), `SELECT 'text'`).Scan(&borrowed); err == nil {
				t.Fatal("single-row scan returned bytes invalidated by its own cursor closure")
			}
			var owned []byte
			if err := access.QueryRowContext(t.Context(), `SELECT 'text'`).Scan(&owned); err != nil || string(owned) != "text" {
				t.Fatalf("owned bytes: value=%q error=%v", owned, err)
			}
		})
		t.Run("canceled query", func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			row := access.QueryRowContext(ctx, `SELECT 1`)
			var value int
			if !errors.Is(row.Err(), context.Canceled) || !errors.Is(row.Scan(&value), context.Canceled) {
				t.Fatalf("query cancellation was not preserved: %v", row.Err())
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHandledCursorErrorsDoNotBecomeConnectionCleanupFailures(t *testing.T) {
	database, _ := readScopeDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := sqlite.RunInTx(ctx, database, func(writer context.Context) error {
		if err := sqlite.RunReadSnapshot(writer, database, func(reader context.Context) error {
			rows, err := sqlite.Connection(reader, database).QueryContext(reader, `SELECT 1 UNION ALL SELECT abs(-9223372036854775808)`)
			if err != nil {
				return err
			}
			defer rows.Close()
			if !rows.Next() || rows.Next() || rows.Err() == nil {
				t.Fatalf("query did not reach its deliberate iteration failure: %v", rows.Err())
			}
			return nil
		}); err != nil {
			return err
		}
		if err := sqlite.RunReadSnapshot(writer, database, func(reader context.Context) error {
			query, cancel := context.WithCancel(reader)
			defer cancel()
			rows, err := sqlite.Connection(reader, database).QueryContext(query, `SELECT 1`)
			if err != nil {
				return err
			}
			cancel()
			for rows.Err() == nil && ctx.Err() == nil {
				runtime.Gosched()
			}
			if !errors.Is(rows.Err(), context.Canceled) {
				t.Fatalf("query did not expose its original cancellation: %v", rows.Err())
			}
			return nil
		}); err != nil {
			return err
		}
		_, err := sqlite.Connection(writer, database).ExecContext(writer, `INSERT INTO facts VALUES('handled cursor errors')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := databaseQuery(database, ctx, `SELECT count(*) FROM facts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("handled statement errors poisoned the original writer: count=%d error=%v", count, err)
	}
}

func TestTransactionsRefuseAnUninitializedDatabase(t *testing.T) {
	database := &sqlite.Database{}
	for _, test := range []struct {
		name  string
		enter func(context.Context, *sqlite.Database, func(context.Context) error) error
	}{
		{name: "write", enter: sqlite.RunInTx},
		{name: "independent", enter: sqlite.RunIndependentTx},
		{name: "read", enter: sqlite.RunReadSnapshot},
	} {
		t.Run(test.name, func(t *testing.T) {
			entered := false
			err := test.enter(t.Context(), database, func(context.Context) error { entered = true; return nil })
			if err == nil || entered {
				t.Fatalf("transaction admitted a connection without original cursor evidence: entered=%t error=%v", entered, err)
			}
		})
	}
}
