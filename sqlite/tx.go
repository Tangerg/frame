package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Tangerg/frame"
)

type txKey struct{ database *databaseOwner }

type transaction struct {
	tx       *transactionAccess
	readOnly bool
}

// Standalone database adapters use this exact database identity to join their
// caller's write-set. Frame Store callbacks receive their scoped executor instead.
func Connection(ctx context.Context, db Database) Executor {
	if db.databaseOwner == nil {
		return &transactionAccess{}
	}
	if owned, ok := ctx.Value(txKey{database: db.databaseOwner}).(transaction); ok {
		return owned.tx
	}
	return &transactionAccess{}
}

// Nested writers join the exact database's original transaction. Taking a
// second connection here could deadlock owners with a single-slot pool.
func RunInTx(ctx context.Context, db Database, fn func(context.Context) error) error {
	if db.databaseOwner == nil || fn == nil {
		return frame.ErrInvalid
	}
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	database := db.databaseOwner
	if owned, ok := ctx.Value(txKey{database: database}).(transaction); ok {
		if err := owned.tx.validate(); err != nil {
			return err
		}
		if owned.readOnly {
			return errors.New("sqlite: write transaction cannot enter a read snapshot")
		}
		return errors.Join(fn(ctx), frame.ContextError(ctx))
	}
	return database.runIndependentTx(ctx, writeTransaction, fn)
}

// RunReadSnapshot reads one consistent snapshot without reserving the WAL writer.
// Reads join an ambient transaction for the same database. The callback must not
// mutate; RunInTx refuses to promote this snapshot to a write-set.
func RunReadSnapshot(ctx context.Context, db Database, read func(context.Context) error) error {
	if db.databaseOwner == nil || read == nil {
		return frame.ErrInvalid
	}
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	database := db.databaseOwner
	if owned, ok := ctx.Value(txKey{database: database}).(transaction); ok {
		owned.readOnly = true
		return owned.tx.read(ctx, func(scope *connectionScope) error {
			access := newTransactionAccess(scope)
			owned.tx = access
			return access.run(func() error {
				return errors.Join(read(context.WithValue(ctx, txKey{database: database}, owned)), frame.ContextError(ctx))
			})
		})
	}
	return database.runIndependentTx(ctx, readTransaction, read)
}

// Independent transactions refuse an ambient write-set on the same database.
// Once BEGIN succeeds, this owner joins the original rollback or commit.
func RunIndependentTx(ctx context.Context, db Database, fn func(context.Context) error) (err error) {
	if db.databaseOwner == nil {
		return frame.ErrInvalid
	}
	return db.databaseOwner.runIndependentTx(ctx, writeTransaction, fn)
}

func (d *databaseOwner) runIndependentTx(ctx context.Context, mode transactionMode, fn func(context.Context) error) error {
	if fn == nil {
		return frame.ErrInvalid
	}
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	if _, active := ctx.Value(txKey{database: d}).(transaction); active {
		return errors.Join(errors.New("sqlite: independent transaction cannot enter an active database transaction"), frame.ContextError(ctx))
	}
	// Unlike Store submissions, standalone transactions have no owned journal
	// to reconcile. Fence before releasing a connection with an unknown commit.
	err := d.transact(ctx, mode, func(executor Executor) error {
		access := executor.(*transactionAccess)
		scoped := context.WithValue(ctx, txKey{database: d}, transaction{tx: access, readOnly: mode == readTransaction})
		return fn(scoped)
	}, d.requireRecovery)
	if errors.Is(err, errCommitUnresolved) {
		d.startClose()
		return errors.Join(ErrIndeterminate, err)
	}
	return err
}

func panicError(recovered any) error {
	if err, ok := recovered.(error); ok {
		return err
	}
	return fmt.Errorf("sqlite: transaction panicked: %v", recovered)
}

func beginTransaction(ctx context.Context, conn *sql.Conn, mode transactionMode) error {
	var timeout int
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return err
	}
	err := waitForSQLite(ctx, time.Duration(timeout)*time.Millisecond, func() error {
		_, err := conn.ExecContext(ctx, mode.beginStatement())
		return err
	})
	_, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA busy_timeout=%d", timeout))
	return errors.Join(err, restoreErr)
}
