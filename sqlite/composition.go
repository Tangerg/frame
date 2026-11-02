package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// Store owns the pool lifetime. Application adapters may access their own
// business tables through it; retained-reference writes join Owner admission.
func (s *Store) Database() *sql.DB { return s.db }

// A callback receives authority over this connection for its original transaction
// only. In particular a *sql.DB cannot stand in for an already admitted write-set.
type transactionAccess struct {
	owner       *Store
	executor    Executor
	mode        transactionMode
	mu          sync.RWMutex
	closedRow   *sql.Row
	openQueries []context.CancelFunc
}

func newTransactionAccess(owner *Store, executor Executor, mode transactionMode) *transactionAccess {
	return &transactionAccess{owner: owner, executor: executor, mode: mode, closedRow: owner.expiredTransactionRow}
}

func (a *transactionAccess) seal() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, end := range a.openQueries {
		end()
	}
	a.openQueries = nil
	a.executor = nil
	a.owner = nil
}

func (a *transactionAccess) ExecContext(ctx context.Context, statement string, args ...any) (sql.Result, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.owner == nil {
		return nil, sql.ErrTxDone
	}
	return a.executor.ExecContext(ctx, statement, args...)
}

func (a *transactionAccess) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.owner == nil {
		return nil, sql.ErrTxDone
	}
	return a.executor.QueryContext(a.rowContextLocked(ctx), statement, args...)
}

func (a *transactionAccess) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.owner == nil {
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

func (s *Store) WithTransaction(ctx context.Context, executor Executor, apply func(context.Context) error) error {
	original, ok := executor.(*transactionAccess)
	if !ok || original == nil || s == nil || ctx == nil || apply == nil {
		return errors.New("sqlite: the original store transaction is required")
	}
	original.mu.RLock()
	owner := original.owner
	original.mu.RUnlock()
	if owner == nil {
		return sql.ErrTxDone
	}
	if owner != s {
		return errors.New("sqlite: foreign store transaction")
	}
	if _, active := ctx.Value(txKey{database: s.db}).(transaction); active {
		return errors.New("sqlite: transaction already bound to this database")
	}
	access := newTransactionAccess(s, original, original.mode)
	defer access.seal()
	return apply(context.WithValue(ctx, txKey{database: s.db}, transaction{tx: access, readOnly: access.mode == readTransaction}))
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
