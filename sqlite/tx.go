package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"github.com/Tangerg/frame/cancellation"
)

// Cross-store transactions. The stores in this package each own their own
// table(s) but share one *sql.DB. Some use cases need a write-set that spans
// several stores atomically, so a mid-sequence failure must leave no partial
// state.
//
// A caller-supplied transaction is carried on the context: [RunInTx] begins it
// (or joins an outer one) and binds it to that exact database; every store statement
// runs through [Connection], which uses that transaction when present and the shared
// pool otherwise. This is mandatory rather than an optimization: the pool runs
// at MaxOpenConns(1), so a store that opened its OWN BeginTx while a context
// transaction was live would deadlock — the single connection is already held.

// Executor is the statement surface shared by *sql.DB and *sql.Tx. Statements
// join a caller transaction only when they name the same database owner.

type txKey struct{ database *sql.DB }

type transaction struct {
	tx       Executor
	readOnly bool
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

// RunInTx runs fn inside a single transaction exposed to fn's store calls via
// the returned context (they pick it up through [Connection]). It commits on success
// and joins rollback on error or panic, so a multi-step write-set is atomic. A
// nested RunInTx joins the outer transaction (a single connection can't nest
// transactions, and the outer owns the commit), so a store method that wraps
// its own body in RunInTx is atomic standalone yet folds into a caller's
// transaction transparently.
func RunInTx(ctx context.Context, db *sql.DB, fn func(context.Context) error) error {
	if err := cancellation.Err(ctx); err != nil {
		return err
	}
	if owned, ok := ctx.Value(txKey{database: db}).(transaction); ok {
		if owned.readOnly {
			return errors.New("sqlite: write transaction cannot enter a read snapshot")
		}
		if err := fn(ctx); err != nil {
			return errors.Join(err, cancellation.Err(ctx))
		}
		return nil
	}
	return RunIndependentTx(ctx, db, fn)
}

// RunReadSnapshot reads one consistent snapshot without reserving the WAL writer.
// Reads join an ambient transaction for the same database. The callback must not
// mutate; RunInTx refuses to promote this snapshot to a write-set.
func RunReadSnapshot(ctx context.Context, db *sql.DB, read func(context.Context) error) error {
	if err := cancellation.Err(ctx); err != nil {
		return err
	}
	if _, ok := ctx.Value(txKey{database: db}).(transaction); ok {
		return errors.Join(read(ctx), cancellation.Err(ctx))
	}
	return runIndependentTx(ctx, db, &sql.TxOptions{ReadOnly: true}, read)
}

// RunIndependentTx owns a fresh write-set and refuses an active transaction for
// the same database. Another database remains an independent fact owner.
// Acquisition and statements retain caller cancellation. The transaction has
// an owned lifetime so database/sql cannot race this boundary's rollback and
// discard its diagnostic. Workspace intents and schema admission deliberately
// enter here rather than joining an ambient write-set.
func RunIndependentTx(ctx context.Context, db *sql.DB, fn func(context.Context) error) (err error) {
	return runIndependentTx(ctx, db, nil, fn)
}

func runIndependentTx(ctx context.Context, db *sql.DB, options *sql.TxOptions, fn func(context.Context) error) (err error) {
	if err := cancellation.Err(ctx); err != nil {
		return err
	}
	if _, active := ctx.Value(txKey{database: db}).(transaction); active {
		return errors.Join(errors.New("sqlite: independent transaction cannot enter an active database transaction"), cancellation.Err(ctx))
	}
	connection, err := db.Conn(ctx)
	if err != nil {
		return errors.Join(err, cancellation.Err(ctx))
	}
	defer func() {
		if closeErr := connection.Close(); !errors.Is(closeErr, sql.ErrConnDone) {
			err = errors.Join(err, closeErr)
		}
	}()
	if err := cancellation.Err(ctx); err != nil {
		return err
	}
	tx, err := connection.BeginTx(context.WithoutCancel(ctx), options)
	if err != nil {
		return errors.Join(err, discardConnection(connection), cancellation.Err(ctx))
	}
	defer func() {
		rollbackErr := rollbackTransaction(tx)
		if rollbackErr != nil {
			rollbackErr = errors.Join(rollbackErr, discardConnection(connection))
		}
		if recovered := recover(); recovered != nil {
			if rollbackErr != nil {
				panicErr, ok := recovered.(error)
				if !ok {
					panicErr = fmt.Errorf("sqlite: transaction panicked: %v", recovered)
				}
				panic(errors.Join(panicErr, rollbackErr, cancellation.Err(ctx)))
			}
			panic(recovered)
		}
		err = errors.Join(err, rollbackErr)
	}()
	if err = cancellation.Err(ctx); err == nil {
		err = fn(context.WithValue(ctx, txKey{database: db}, transaction{tx: tx, readOnly: options != nil && options.ReadOnly}))
	}
	err = errors.Join(err, cancellation.Err(ctx))
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(err, discardConnection(connection), cancellation.Err(ctx))
	}
	return nil
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
