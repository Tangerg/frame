package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	modernc "modernc.org/sqlite"
)

type cursorFaultRows struct {
	sqliteRows
	beforeNext  func()
	beforeClose func()
	afterClose  func() error
}

func (r *cursorFaultRows) Next(values []driver.Value) error {
	if r.beforeNext != nil {
		r.beforeNext()
	}
	return r.sqliteRows.Next(values)
}

func (r *cursorFaultRows) Close() error {
	if r.beforeClose != nil {
		r.beforeClose()
	}
	err := r.sqliteRows.Close()
	if r.afterClose != nil {
		err = errors.Join(err, r.afterClose())
	}
	return err
}

func cursorFaultDatabase(t *testing.T, fault transactionFault) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cursors.db")
	seed, err := OpenSchema(t.Context(), path, []string{`CREATE TABLE facts (value TEXT NOT NULL)`})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	database := sql.OpenDB(cursorConnector{Connector: &faultConnector{driver: &modernc.Driver{}, dsn: storeDataSource(path), fault: fault}})
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	return database
}

func TestReadSnapshotJoinsOriginalNextAndCloseBeforeRestoringWriter(t *testing.T) {
	const statement = `SELECT 'original cursor'`
	nextEntered, releaseNext := make(chan struct{}), make(chan struct{})
	closeEntered, releaseClose := make(chan struct{}), make(chan struct{})
	unblockNext := sync.OnceFunc(func() { close(releaseNext) })
	unblockClose := sync.OnceFunc(func() { close(releaseClose) })
	var readerEntered, writerRestored atomic.Bool
	database := cursorFaultDatabase(t, transactionFault{
		queryRows: func(_ context.Context, query string, rows driver.Rows) driver.Rows {
			if query != statement {
				return rows
			}
			return &cursorFaultRows{
				sqliteRows:  rows.(sqliteRows),
				beforeNext:  func() { close(nextEntered); <-releaseNext },
				beforeClose: func() { close(closeEntered); <-releaseClose },
			}
		},
		afterAuthorize: func(authorizer modernc.AuthorizerFn) error {
			if readerEntered.Load() && authorizer != nil && authorizer(modernc.AuthInsert, "", "", "", "") == modernc.AuthorizerOK {
				writerRestored.Store(true)
			}
			return nil
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	accessReady := make(chan *transactionAccess, 1)
	nextDone, completed := make(chan bool, 1), make(chan error, 1)
	go func() {
		completed <- RunInTx(ctx, database, func(writer context.Context) error {
			if err := RunReadSnapshot(writer, database, func(reader context.Context) error {
				readerEntered.Store(true)
				access := Connection(reader, database).(*transactionAccess)
				accessReady <- access
				rows, err := access.QueryContext(reader, statement)
				if err != nil {
					return err
				}
				go func() { nextDone <- rows.Next() }()
				<-nextEntered
				return nil
			}); err != nil {
				return err
			}
			_, err := Connection(writer, database).ExecContext(writer, `INSERT INTO facts VALUES('writer resumed')`)
			return err
		})
	}()
	finished := false
	defer func() {
		unblockNext()
		unblockClose()
		if !finished {
			<-completed
		}
	}()
	var access *transactionAccess
	select {
	case access = <-accessReady:
	case err := <-completed:
		finished = true
		t.Fatalf("read callback did not start: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-access.done:
	case <-ctx.Done():
		t.Fatal("read callback did not seal its query authority")
	}
	if writerRestored.Load() {
		t.Fatal("write authority overtook an executing Next")
	}
	select {
	case err := <-completed:
		finished = true
		t.Fatalf("owner returned before Next exited: %v", err)
	default:
	}
	unblockNext()
	select {
	case <-closeEntered:
	case <-ctx.Done():
		t.Fatal("owner did not join its original cursor close")
	}
	if writerRestored.Load() {
		t.Fatal("write authority overtook the original cursor close")
	}
	unblockClose()
	if err := <-completed; err != nil {
		finished = true
		t.Fatal(err)
	}
	finished = true
	if !<-nextDone || !writerRestored.Load() {
		t.Fatal("joined cursor did not return authority to its original writer")
	}
}

func TestCursorCloseFailureRemainsWithOriginalTransaction(t *testing.T) {
	const statement = `SELECT 'cursor close failure'`
	for _, finish := range []string{"unscanned", "caught panic", "handled scan error"} {
		t.Run(finish, func(t *testing.T) {
			closeCause := errors.New("cursor close acknowledgement lost")
			panicCause := errors.New("reader panicked")
			var disposed atomic.Int32
			database := cursorFaultDatabase(t, transactionFault{
				queryRows: func(_ context.Context, query string, rows driver.Rows) driver.Rows {
					if query != statement {
						return rows
					}
					return &cursorFaultRows{sqliteRows: rows.(sqliteRows), afterClose: func() error { return closeCause }}
				},
				afterClose: func() { disposed.Add(1) },
			})
			var nestedErr error
			err := RunInTx(t.Context(), database, func(writer context.Context) error {
				if _, err := Connection(writer, database).ExecContext(writer, `INSERT INTO facts VALUES('must rollback')`); err != nil {
					return err
				}
				recovered := capturePanic(func() {
					nestedErr = RunReadSnapshot(writer, database, func(reader context.Context) error {
						access := Connection(reader, database)
						if finish == "handled scan error" {
							var value string
							if err := access.QueryRowContext(reader, statement).Scan(&value); !errors.Is(err, closeCause) {
								t.Fatalf("scan did not preserve cursor close failure: %v", err)
							}
							return nil
						}
						if _, err := access.QueryContext(reader, statement); err != nil {
							return err
						}
						if finish == "caught panic" {
							panic(panicCause)
						}
						return nil
					})
				})
				if finish == "caught panic" {
					failure, ok := recovered.(error)
					if !ok || !errors.Is(failure, panicCause) || !errors.Is(failure, closeCause) {
						t.Fatalf("panic lost cursor cleanup evidence: %v", recovered)
					}
				} else if !errors.Is(nestedErr, closeCause) {
					t.Fatalf("nested reader lost cursor cleanup evidence: %v", nestedErr)
				}
				return nil
			})
			if !errors.Is(err, closeCause) || !errors.Is(err, errCursorCloseUnconfirmed) || disposed.Load() != 1 {
				t.Fatalf("caught failure erased original cleanup obligation: error=%v closes=%d", err, disposed.Load())
			}
			var count int
			if err := database.QueryRowContext(t.Context(), `SELECT count(*) FROM facts`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unconfirmed cursor committed its enclosing transaction: count=%d error=%v", count, err)
			}
		})
	}
}

func TestAutomaticAndCallerCursorCloseKeepOriginalReleaseEvidence(t *testing.T) {
	closeCause := errors.New("original cursor close is unconfirmed")
	for _, test := range []struct {
		name      string
		statement string
		consume   func(*testing.T, *sql.Rows, context.CancelFunc, <-chan struct{})
	}{
		{name: "explicit", statement: `SELECT 1`, consume: func(t *testing.T, rows *sql.Rows, _ context.CancelFunc, _ <-chan struct{}) {
			if err := rows.Close(); !errors.Is(err, closeCause) {
				t.Fatalf("original Close result: %v", err)
			}
			if err := rows.Close(); err != nil {
				t.Fatalf("database/sql did not make repeated Close a no-op: %v", err)
			}
		}},
		{name: "deferred", statement: `SELECT 1`, consume: func(t *testing.T, rows *sql.Rows, _ context.CancelFunc, _ <-chan struct{}) {
			defer rows.Close()
			if !rows.Next() || rows.Err() != nil {
				t.Fatalf("row before deferred Close: %v", rows.Err())
			}
		}},
		{name: "exhausted", statement: `SELECT 1`, consume: func(t *testing.T, rows *sql.Rows, _ context.CancelFunc, _ <-chan struct{}) {
			for rows.Next() {
			}
			if !errors.Is(rows.Err(), closeCause) {
				t.Fatalf("EOF did not invoke original Close: %v", rows.Err())
			}
		}},
		{name: "canceled", statement: `SELECT 1`, consume: func(t *testing.T, rows *sql.Rows, cancel context.CancelFunc, closed <-chan struct{}) {
			cancel()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("query cancellation did not close its physical cursor")
			}
			if !errors.Is(rows.Err(), context.Canceled) {
				t.Fatalf("query cancellation disappeared: %v", rows.Err())
			}
		}},
		{name: "iteration and close failure", statement: `SELECT 1 UNION ALL SELECT abs(-9223372036854775808)`, consume: func(t *testing.T, rows *sql.Rows, _ context.CancelFunc, _ <-chan struct{}) {
			if !rows.Next() || rows.Next() || rows.Err() == nil {
				t.Fatalf("iteration did not reach its error: %v", rows.Err())
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			closed := make(chan struct{})
			var disposed, cursorCloses atomic.Int32
			database := cursorFaultDatabase(t, transactionFault{
				queryRows: func(_ context.Context, query string, rows driver.Rows) driver.Rows {
					if query != test.statement {
						return rows
					}
					return &cursorFaultRows{sqliteRows: rows.(sqliteRows), afterClose: func() error {
						cursorCloses.Add(1)
						close(closed)
						return closeCause
					}}
				},
				afterClose: func() { disposed.Add(1) },
			})
			err := RunInTx(t.Context(), database, func(writer context.Context) error {
				if _, err := Connection(writer, database).ExecContext(writer, `INSERT INTO facts VALUES('must rollback')`); err != nil {
					return err
				}
				err := RunReadSnapshot(writer, database, func(reader context.Context) error {
					query, cancel := context.WithCancel(reader)
					defer cancel()
					rows, err := Connection(reader, database).QueryContext(query, test.statement)
					if err != nil {
						return err
					}
					test.consume(t, rows, cancel, closed)
					return nil
				})
				if !errors.Is(err, closeCause) {
					t.Fatalf("nested reader forgot its original release: %v", err)
				}
				return nil
			})
			if !errors.Is(err, closeCause) || !errors.Is(err, errCursorCloseUnconfirmed) || disposed.Load() != 1 || cursorCloses.Load() != 1 {
				t.Fatalf("original cursor failure was lost: error=%v disposed=%d cursor closes=%d", err, disposed.Load(), cursorCloses.Load())
			}
			var count int
			if err := database.QueryRowContext(t.Context(), `SELECT count(*) FROM facts`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unconfirmed cursor committed its writer: count=%d error=%v", count, err)
			}
		})
	}
}
