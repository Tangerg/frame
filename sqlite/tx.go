package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

type txKey struct{ database *sql.DB }

type transaction struct {
	tx       Executor
	readOnly bool
}

func (t transaction) checkActive() error {
	if access, ok := t.tx.(*transactionAccess); ok {
		access.mu.RLock()
		defer access.mu.RUnlock()
		if access.owner == nil {
			return sql.ErrTxDone
		}
	}
	return nil
}

// Connection returns the transaction carried by [RunInTx] for this exact database,
// else the shared pool db. A store method MUST route through this (not the bare
// s.db) for any statement that should join a caller's cross-store transaction.
func Connection(ctx context.Context, db *sql.DB) Executor {
	if owned, ok := ctx.Value(txKey{database: db}).(transaction); ok {
		return owned.tx
	}
	return db
}

// Nested writers join the exact database's original transaction. Taking a
// second connection here could deadlock owners with a single-slot pool.
func RunInTx(ctx context.Context, db *sql.DB, fn func(context.Context) error) error {
	if err := cancellationError(ctx); err != nil {
		return err
	}
	if owned, ok := ctx.Value(txKey{database: db}).(transaction); ok {
		if err := owned.checkActive(); err != nil {
			return err
		}
		if owned.readOnly {
			return errors.New("sqlite: write transaction cannot enter a read snapshot")
		}
		if err := fn(ctx); err != nil {
			return errors.Join(err, cancellationError(ctx))
		}
		return nil
	}
	return RunIndependentTx(ctx, db, fn)
}

// RunReadSnapshot reads one consistent snapshot without reserving the WAL writer.
// Reads join an ambient transaction for the same database. The callback must not
// mutate; RunInTx refuses to promote this snapshot to a write-set.
func RunReadSnapshot(ctx context.Context, db *sql.DB, read func(context.Context) error) error {
	if err := cancellationError(ctx); err != nil {
		return err
	}
	if owned, ok := ctx.Value(txKey{database: db}).(transaction); ok {
		if err := owned.checkActive(); err != nil {
			return err
		}
		if owned.readOnly {
			return errors.Join(read(ctx), cancellationError(ctx))
		}
		owned.readOnly = true
		return withQueryOnly(ctx, owned.tx, func() error {
			return errors.Join(read(context.WithValue(ctx, txKey{database: db}, owned)), cancellationError(ctx))
		})
	}
	return runIndependentTx(ctx, db, &sql.TxOptions{ReadOnly: true}, read)
}

// Independent transactions refuse an ambient write-set on the same database.
// Once BEGIN succeeds, this owner joins the original rollback or commit.
func RunIndependentTx(ctx context.Context, db *sql.DB, fn func(context.Context) error) (err error) {
	return runIndependentTx(ctx, db, nil, fn)
}

func runIndependentTx(ctx context.Context, db *sql.DB, options *sql.TxOptions, fn func(context.Context) error) (err error) {
	if err := cancellationError(ctx); err != nil {
		return err
	}
	if _, active := ctx.Value(txKey{database: db}).(transaction); active {
		return errors.Join(errors.New("sqlite: independent transaction cannot enter an active database transaction"), cancellationError(ctx))
	}
	connection, err := db.Conn(ctx)
	if err != nil {
		return errors.Join(err, cancellationError(ctx))
	}
	defer func() {
		if closeErr := connection.Close(); !errors.Is(closeErr, sql.ErrConnDone) {
			err = errors.Join(err, closeErr)
		}
	}()
	if err := cancellationError(ctx); err != nil {
		return err
	}
	tx, err := beginTransaction(ctx, connection, options)
	if err != nil {
		return errors.Join(err, discardConnection(connection), cancellationError(ctx))
	}
	defer func() {
		rollbackErr := rollbackTransaction(tx)
		if rollbackErr != nil || errors.Is(err, errReadModeUnconfirmed) {
			rollbackErr = errors.Join(rollbackErr, discardConnection(connection))
		}
		if recovered := recover(); recovered != nil {
			if rollbackErr != nil {
				panicErr, ok := recovered.(error)
				if !ok {
					panicErr = fmt.Errorf("sqlite: transaction panicked: %v", recovered)
				}
				panic(errors.Join(panicErr, rollbackErr, cancellationError(ctx)))
			}
			panic(recovered)
		}
		err = errors.Join(err, rollbackErr)
	}()
	if err = cancellationError(ctx); err == nil {
		readOnly := options != nil && options.ReadOnly
		scoped := context.WithValue(ctx, txKey{database: db}, transaction{tx: tx, readOnly: readOnly})
		if readOnly {
			err = withQueryOnly(scoped, tx, func() error { return fn(scoped) })
		} else {
			err = fn(scoped)
		}
	}
	err = errors.Join(err, cancellationError(ctx))
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(err, discardConnection(connection), cancellationError(ctx))
	}
	return nil
}

var errReadModeUnconfirmed = errors.New("sqlite: connection read mode could not be restored")

// modernc's ReadOnly option selects a deferred BEGIN. SQLite's query_only
// setting enforces the read scope even when a trusted adapter issues DML directly.
func withQueryOnly(ctx context.Context, executor Executor, read func() error) (err error) {
	var previous int
	if err := executor.QueryRowContext(ctx, "PRAGMA query_only").Scan(&previous); err != nil {
		return err
	}
	if previous != 0 {
		return read()
	}
	if _, err := executor.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return err
	}
	defer func() {
		if _, restoreErr := executor.ExecContext(context.WithoutCancel(ctx), "PRAGMA query_only=OFF"); restoreErr != nil {
			err = errors.Join(err, errReadModeUnconfirmed, restoreErr)
		}
	}()
	return read()
}

func beginTransaction(ctx context.Context, conn *sql.Conn, options *sql.TxOptions) (*sql.Tx, error) {
	var timeout int
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return nil, err
	}
	var tx *sql.Tx
	err := waitForSQLite(ctx, time.Duration(timeout)*time.Millisecond, func() error {
		var err error
		// Each BEGIN is nonblocking. Its lifetime belongs to the caller of
		// Commit/Rollback once it wins the writer slot.
		tx, err = conn.BeginTx(context.WithoutCancel(ctx), options)
		return err
	})
	var executor Executor = conn
	if tx != nil {
		executor = tx
	}
	_, restoreErr := executor.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA busy_timeout=%d", timeout))
	if restoreErr != nil && tx != nil {
		restoreErr = errors.Join(restoreErr, tx.Rollback())
	}
	if err != nil || restoreErr != nil {
		return nil, errors.Join(err, restoreErr)
	}
	return tx, nil
}

func rollbackTransaction(tx *sql.Tx) error {
	err := tx.Rollback()
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

// A failed transaction boundary cannot prove the connection is reusable.
// Raw's ErrBadConn contract discards it instead of returning it to the pool.
func discardConnection(connection *sql.Conn) error {
	err := connection.Raw(func(any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return nil
	}
	return err
}
