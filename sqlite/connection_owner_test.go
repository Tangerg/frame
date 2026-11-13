package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	modernc "modernc.org/sqlite"
)

type trackedConnector struct {
	*faultConnector
	original *faultConnection
	opened   int
}

func (c *trackedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := c.faultConnector.Connect(ctx)
	if err == nil {
		c.original = connection.(*faultConnection)
		c.opened++
	}
	return connection, err
}

func TestStandaloneRetainsOriginalConnectionUntilPhysicalClose(t *testing.T) {
	for _, phase := range []string{"begin", "commit", "rollback", "policy"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "original.db")
			seed, err := OpenSchema(t.Context(), path, []string{`CREATE TABLE facts (value TEXT)`})
			if err != nil {
				t.Fatal(err)
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}
			boundaryCause := errors.New("original transaction boundary failed")
			closeCause := errors.New("original physical close failed")
			var allowClose atomic.Bool
			fault := transactionFault{}
			switch phase {
			case "begin":
				fault.beforeExec = func(query string) error {
					if query == "BEGIN IMMEDIATE" {
						return boundaryCause
					}
					return nil
				}
			case "commit":
				fault.afterExec = func(query string) error {
					if query == "COMMIT" {
						return boundaryCause
					}
					return nil
				}
			case "rollback":
				fault.beforeExec = func(query string) error {
					if query == "ROLLBACK" {
						return boundaryCause
					}
					return nil
				}
			case "policy":
				fault = statementPolicyRestoreFault(false, boundaryCause)
			}
			fault.beforeClose = func() error {
				if !allowClose.Load() {
					return closeCause
				}
				return nil
			}
			connector := &trackedConnector{faultConnector: &faultConnector{driver: &modernc.Driver{}, dsn: storeDataSource(path), fault: fault}}
			database := &Database{databaseOwner: &databaseOwner{db: sql.OpenDB(cursorConnector{Connector: connector})}}
			database.db.SetMaxOpenConns(1)
			t.Cleanup(func() {
				allowClose.Store(true)
				if err := database.Close(); err != nil {
					t.Error(err)
				}
			})
			err = RunInTx(t.Context(), database, func(ctx context.Context) error {
				if phase == "policy" {
					_ = RunReadSnapshot(ctx, database, func(context.Context) error { return nil })
				}
				if phase == "rollback" {
					return boundaryCause
				}
				return nil
			})
			if !errors.Is(err, boundaryCause) || !errors.Is(err, closeCause) || !errors.Is(err, ErrIndeterminate) {
				t.Fatalf("cleanup lost original evidence: %v", err)
			}
			if err := RunInTx(t.Context(), database, func(context.Context) error { t.Fatal("sealed database admitted work"); return nil }); !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("unconfirmed owner remained open: %v", err)
			}
			if err := database.Close(); !errors.Is(err, closeCause) {
				t.Fatalf("close replaced original failure: %v", err)
			}
			if connector.opened != 1 || connector.original.sqliteConnection.Ping(t.Context()) != nil || len(database.unconfirmed) != 1 {
				t.Fatal("original physical connection was replaced or abandoned")
			}
			allowClose.Store(true)
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			if !connector.original.closed || connector.opened != 1 || len(database.unconfirmed) != 0 {
				t.Fatal("close did not settle the original connection")
			}
		})
	}
}

func TestStandaloneCloseJoinsAdmittedTransaction(t *testing.T) {
	database, err := OpenSchema(t.Context(), ":memory:", []string{`CREATE TABLE facts (value TEXT)`})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- RunInTx(t.Context(), database, func(ctx context.Context) error {
			close(entered)
			<-release
			_, err := Connection(ctx, database).ExecContext(ctx, `INSERT INTO facts VALUES ('original')`)
			return err
		})
	}()
	<-entered
	database.startClose()
	closing := make(chan error, 1)
	go func() { closing <- database.Close() }()
	if err := RunReadSnapshot(t.Context(), database, func(context.Context) error { t.Error("new read entered closing database"); return nil }); !errors.Is(err, frame.ErrClosed) {
		t.Errorf("closing admission: %v", err)
	}
	select {
	case err := <-closing:
		close(release)
		<-finished
		t.Fatalf("close bypassed original transaction: %v", err)
	default:
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-closing; err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneConnectionRequiresOriginalTransaction(t *testing.T) {
	database, err := OpenSchema(t.Context(), ":memory:", []string{`CREATE TABLE facts (value TEXT)`})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := Connection(t.Context(), database).ExecContext(t.Context(), `INSERT INTO facts VALUES ('outside')`); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("unscoped executor admitted work: %v", err)
	}
}

func TestDatabaseCopiesJoinTheOriginalTransaction(t *testing.T) {
	database, err := OpenSchema(t.Context(), ":memory:", []string{`CREATE TABLE facts (value TEXT)`})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	copy := *database
	if err := RunInTx(t.Context(), database, func(ctx context.Context) error {
		return RunInTx(ctx, &copy, func(ctx context.Context) error {
			_, err := Connection(ctx, &copy).ExecContext(ctx, `INSERT INTO facts VALUES ('original')`)
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Read(t.Context(), func(Reader) error { t.Fatal("copy close did not close original owner"); return nil }); !errors.Is(err, frame.ErrClosed) {
		t.Fatal(err)
	}
}

func TestUnresolvedCommitClosesItsOriginalDatabaseAfterHandleReplacement(t *testing.T) {
	cause := errors.New("original commit acknowledgement lost")
	database := cursorFaultDatabase(t, transactionFault{afterExec: func(query string) error {
		if query == "COMMIT" {
			return cause
		}
		return nil
	}})
	original := *database
	defer original.Close()
	replacement, err := OpenSchema(t.Context(), ":memory:", []string{`CREATE TABLE facts (value TEXT)`})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	err = RunIndependentTx(t.Context(), database, func(ctx context.Context) error {
		if _, err := Connection(ctx, database).ExecContext(ctx, `INSERT INTO facts VALUES ('original')`); err != nil {
			return err
		}
		*database = *replacement
		return nil
	})
	if !errors.Is(err, cause) || !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("original commit lost its evidence: %v", err)
	}
	if err := RunInTx(t.Context(), replacement, func(ctx context.Context) error {
		_, err := Connection(ctx, replacement).ExecContext(ctx, `INSERT INTO facts VALUES ('replacement')`)
		return err
	}); err != nil {
		t.Fatalf("original commit sealed an unrelated database: %v", err)
	}
	if !errors.Is(original.transactionFailure(), cause) {
		t.Fatal("original database escaped its recovery boundary")
	}
}

func TestStandaloneUnresolvedCommitFencesQueuedTransactions(t *testing.T) {
	committing, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	cause := errors.New("commit acknowledgement lost")
	var commitIntercepted, closeObserved, fencedBeforeClose atomic.Bool
	var database *Database
	database = cursorFaultDatabase(t, transactionFault{
		afterExec: func(query string) error {
			if query == "COMMIT" && commitIntercepted.CompareAndSwap(false, true) {
				close(committing)
				<-release
				return cause
			}
			return nil
		},
		beforeClose: func() error {
			if closeObserved.CompareAndSwap(false, true) {
				fencedBeforeClose.Store(errors.Is(database.transactionFailure(), frame.ErrClosed))
			}
			return nil
		},
	})
	original := make(chan error, 1)
	go func() { original <- RunInTx(t.Context(), database, func(context.Context) error { return nil }) }()
	<-committing
	queued := make(chan error, 1)
	go func() {
		queued <- RunInTx(t.Context(), database, func(context.Context) error {
			t.Error("queued callback entered after an unresolved commit")
			return nil
		})
	}()
	deadline := time.After(5 * time.Second)
	for database.db.Stats().WaitCount == 0 {
		select {
		case <-deadline:
			t.Fatal("second transaction did not queue behind the original")
		default:
			runtime.Gosched()
		}
	}
	unblock()
	if err := <-original; !errors.Is(err, cause) || !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("unresolved original outcome: %v", err)
	}
	if err := <-queued; !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("queued transaction was not fenced: %v", err)
	}
	if !fencedBeforeClose.Load() {
		t.Fatal("original connection was released before admission was fenced")
	}
}

func TestStandaloneRollbackPreservesCallbackAndCancellationCauses(t *testing.T) {
	database, err := OpenSchema(t.Context(), ":memory:", []string{`CREATE TABLE facts (value TEXT)`})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	callbackCause := errors.New("callback rejected")
	cancellationCause := errors.New("caller canceled")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	err = RunInTx(ctx, database, func(ctx context.Context) error {
		if _, err := Connection(ctx, database).ExecContext(ctx, `INSERT INTO facts VALUES ('original')`); err != nil {
			return err
		}
		cancel(cancellationCause)
		return callbackCause
	})
	if !errors.Is(err, callbackCause) || !errors.Is(err, cancellationCause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("original failure causes were lost: %v", err)
	}
	var count int
	if err := database.Read(t.Context(), func(reader Reader) error {
		return reader.QueryRowContext(t.Context(), `SELECT count(*) FROM facts`).Scan(&count)
	}); err != nil || count != 0 {
		t.Fatalf("original transaction was not rolled back: count=%d error=%v", count, err)
	}
}

func TestStandaloneRejectsNilCallbacksAtNestedBoundary(t *testing.T) {
	database, err := OpenSchema(t.Context(), ":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := RunInTx(t.Context(), database, func(ctx context.Context) error {
		for _, err := range []error{RunInTx(ctx, database, nil), RunReadSnapshot(ctx, database, nil), RunIndependentTx(ctx, database, nil)} {
			if !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("nil callback was admitted: %v", err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
