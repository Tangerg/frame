package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// A callback receives authority over this connection for its original transaction
// only. In particular a *sql.DB cannot stand in for an already admitted write-set.
type transactionAccess struct {
	executor    Executor
	mu          sync.RWMutex
	closedRow   *sql.Row
	openQueries []context.CancelFunc
}

func newTransactionAccess(owner *Store, executor Executor) *transactionAccess {
	return &transactionAccess{executor: executor, closedRow: owner.expiredTransactionRow}
}

func (a *transactionAccess) seal() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, end := range a.openQueries {
		end()
	}
	a.openQueries = nil
	a.executor = nil
}

func (a *transactionAccess) ExecContext(ctx context.Context, statement string, args ...any) (sql.Result, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.executor == nil {
		return nil, sql.ErrTxDone
	}
	return a.executor.ExecContext(ctx, statement, args...)
}

func (a *transactionAccess) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.executor == nil {
		return nil, sql.ErrTxDone
	}
	return a.executor.QueryContext(a.rowContextLocked(ctx), statement, args...)
}

func (a *transactionAccess) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.executor == nil {
		return a.closedRow
	}
	return a.executor.QueryRowContext(a.rowContextLocked(ctx), statement, args...)
}

// database/sql may step a query later through Rows.Next or Row.Scan. A cursor
// retained by an adapter must lose its original statement at scope exit.
func (a *transactionAccess) rowContextLocked(ctx context.Context) context.Context {
	child, cancel := context.WithCancel(ctx)
	a.openQueries = append(a.openQueries, cancel)
	return child
}

// The narrow view excludes write and pool-lifetime authority even through a type
// assertion. Query-only mode also rejects writes expressed as SQL queries.
type readAccess struct{ access Reader }

func (r readAccess) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	return r.access.QueryContext(ctx, statement, args...)
}

func (r readAccess) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	return r.access.QueryRowContext(ctx, statement, args...)
}

// Update admits a transition whose original business row already identifies
// the operation. The row's compare-and-set predicate determines if it can advance.
func (o *Owner) Update(ctx context.Context, apply func(context.Context, Executor) error) error {
	if apply == nil {
		return errors.New("sqlite: business update is required")
	}
	return o.store.admission.Admit(ctx, func(admitted context.Context) error {
		err := o.store.transaction(admitted, func(tx Executor) error { return apply(admitted, tx) })
		if errors.Is(err, errCommitUnresolved) {
			err = errors.Join(ErrIndeterminate, err)
		}
		if errors.Is(err, ErrIndeterminate) {
			o.store.admission.RequireRecovery(err)
		}
		return err
	})
}
