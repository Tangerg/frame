package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	modernc "modernc.org/sqlite"
)

func statementPolicyRestoreFault(after bool, failure error) transactionFault {
	reading := false
	reject := func(authorizer modernc.AuthorizerFn) error {
		if authorizer == nil {
			return nil
		}
		if authorizer(modernc.AuthInsert, "", "", "", "") == modernc.AuthorizerDeny {
			reading = true
			return nil
		}
		if reading {
			reading = false
			return failure
		}
		return nil
	}
	if after {
		return transactionFault{afterAuthorize: reject}
	}
	return transactionFault{beforeAuthorize: reject}
}

func capturePanic(read func()) (recovered any) {
	defer func() { recovered = recover() }()
	read()
	return nil
}

func TestStoreReadPanicPreservesUnconfirmedPolicyCleanup(t *testing.T) {
	for _, after := range []bool{false, true} {
		name := "restore-failed"
		if after {
			name = "restore-acknowledgement-lost"
		}
		t.Run(name, func(t *testing.T) {
			store, owner, _ := testStore(t)
			panicCause := errors.New("reader panicked")
			restoreCause := errors.New("statement policy restoration is unconfirmed")
			var closed atomic.Int32
			fault := statementPolicyRestoreFault(after, restoreCause)
			fault.afterClose = func() { closed.Add(1) }
			installTransactionFault(t, store, fault)
			recovered := capturePanic(func() {
				_ = store.Read(t.Context(), func(Reader) error { panic(panicCause) })
			})
			failure, ok := recovered.(error)
			if !ok || !errors.Is(failure, panicCause) || !errors.Is(failure, restoreCause) || !errors.Is(failure, errStatementPolicyUnconfirmed) {
				t.Fatalf("panic lost its connection cleanup evidence: %v", recovered)
			}
			if closed.Load() != 1 {
				t.Fatalf("unconfirmed read mode returned its original connection to the pool: closes=%d", closed.Load())
			}
			if _, err := owner.Prepare(t.Context(), []byte("next write")); err != nil {
				t.Fatalf("read panic poisoned the next write: %v", err)
			}
		})
	}
}

func TestNestedReadPanicCannotEraseOriginalDiscardObligation(t *testing.T) {
	for _, after := range []bool{false, true} {
		name := "restore-failed"
		if after {
			name = "restore-acknowledgement-lost"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "owned.db")
			seed, err := OpenSchema(t.Context(), path, []string{`CREATE TABLE facts (value TEXT NOT NULL)`})
			if err != nil {
				t.Fatal(err)
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}
			panicCause := errors.New("nested reader panicked")
			restoreCause := errors.New("nested statement policy restoration is unconfirmed")
			var closed atomic.Int32
			fault := statementPolicyRestoreFault(after, restoreCause)
			fault.afterClose = func() { closed.Add(1) }
			database := sql.OpenDB(cursorConnector{Connector: &faultConnector{driver: &modernc.Driver{}, dsn: storeDataSource(path), fault: fault}})
			database.SetMaxOpenConns(1)
			database.SetMaxIdleConns(1)
			defer database.Close()
			var recovered any
			err = RunInTx(t.Context(), database, func(ctx context.Context) error {
				if _, err := Connection(ctx, database).ExecContext(ctx, `INSERT INTO facts VALUES('must rollback')`); err != nil {
					return err
				}
				recovered = capturePanic(func() {
					_ = RunReadSnapshot(ctx, database, func(context.Context) error { panic(panicCause) })
				})
				return nil
			})
			failure, ok := recovered.(error)
			if !ok || !errors.Is(failure, panicCause) || !errors.Is(failure, restoreCause) {
				t.Fatalf("nested panic lost its original evidence: %v", recovered)
			}
			if !errors.Is(err, errStatementPolicyUnconfirmed) || !errors.Is(err, restoreCause) || closed.Load() != 1 {
				t.Fatalf("caught panic erased the original owner's cleanup obligation: error=%v closes=%d", err, closed.Load())
			}
			var count int
			if err := database.QueryRowContext(t.Context(), `SELECT count(*) FROM facts`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("tainted transaction committed: count=%d error=%v", count, err)
			}
			if err := RunInTx(t.Context(), database, func(ctx context.Context) error {
				_, err := Connection(ctx, database).ExecContext(ctx, `INSERT INTO facts VALUES('next write')`)
				return err
			}); err != nil {
				t.Fatalf("nested read mode poisoned the next owner: %v", err)
			}
		})
	}
}
