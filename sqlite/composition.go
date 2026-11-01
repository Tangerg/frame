package sqlite

import (
	"context"
	"database/sql"
	"errors"
)

// Database is for trusted application composition of business-table adapters.
// Store still owns its connection lifetime and exclusive authority. Applications
// must not close this handle or read or write Frame's private tables directly.
// Reference-increasing writes use Owner.Commit, not an independent transaction.
func (s *Store) Database() *sql.DB { return s.db }

// WithTransaction joins existing application adapters to the original Executor
// supplied by Owner.Commit, a reference reader or an installation projection.
// Connection and nested RunInTx see that exact transaction; this function never
// begins, commits or rolls it back. Its context cannot outlive the callback.
func (s *Store) WithTransaction(ctx context.Context, executor Executor, apply func(context.Context) error) error {
	if s == nil || s.db == nil || executor == nil || apply == nil {
		return errors.New("sqlite: store, executor and transaction body are required")
	}
	if _, active := ctx.Value(txKey{database: s.db}).(transaction); active {
		return errors.New("sqlite: an executor is already bound to this database")
	}
	return apply(context.WithValue(ctx, txKey{database: s.db}, transaction{tx: executor}))
}
