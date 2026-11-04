package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

var errCursorCloseUnconfirmed = errors.New("sqlite: cursor release could not be confirmed")

type sqlExecutor interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type ownedCursor struct {
	rows    *sql.Rows
	cancel  context.CancelFunc
	release *cursorRelease
}

func (c *ownedCursor) close() error {
	_ = c.rows.Close()
	c.cancel()
	return c.release.result()
}

// A callback receives authority over this connection for its original transaction
// only. In particular a *sql.DB cannot stand in for an already admitted write-set.
type transactionAccess struct {
	executor sqlExecutor
	scope    *connectionScope
	mu       sync.RWMutex
	cursors  []*ownedCursor
	done     chan struct{}
	once     sync.Once
	closeErr error
}

func newTransactionAccess(scope *connectionScope) *transactionAccess {
	return &transactionAccess{executor: scope.executor, scope: scope, done: make(chan struct{})}
}

func (a *transactionAccess) run(apply func() error) (err error) {
	defer func() {
		recovered := recover()
		closeErr := a.seal()
		if recovered != nil {
			if closeErr != nil {
				panic(errors.Join(panicError(recovered), closeErr))
			}
			panic(recovered)
		}
		err = errors.Join(err, closeErr)
	}()
	return apply()
}

func (a *transactionAccess) seal() error {
	a.once.Do(func() {
		a.mu.Lock()
		a.executor = nil
		close(a.done)
		scope := a.scope
		a.scope = nil
		cursors := a.cursors
		a.cursors = nil
		a.mu.Unlock()
		// Modernc does not observe query cancellation during Rows.Next. Close
		// joins that original call before its connection regains broader authority;
		// canceling first could also hide Close's error behind context.Canceled.
		for _, cursor := range cursors {
			a.closeErr = errors.Join(a.closeErr, cursor.close())
		}
		if a.closeErr != nil {
			a.closeErr = errors.Join(errCursorCloseUnconfirmed, a.closeErr)
			scope.requireDiscard(a.closeErr)
		}
	})
	return a.closeErr
}

func (a *transactionAccess) validate() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.executor == nil {
		return sql.ErrTxDone
	}
	return nil
}

func (a *transactionAccess) read(ctx context.Context, read func(*connectionScope) error) (err error) {
	// A child joins the original connection before its parent can return the grant.
	a.mu.RLock()
	scope := a.scope
	if a.executor == nil {
		err = sql.ErrTxDone
	} else {
		err = scope.acquireRead(ctx)
	}
	a.mu.RUnlock()
	if err != nil {
		return err
	}
	defer func() {
		recovered := recover()
		releaseErr := scope.releaseRead()
		if recovered != nil {
			if releaseErr != nil {
				panic(errors.Join(panicError(recovered), releaseErr))
			}
			panic(recovered)
		}
		err = errors.Join(err, releaseErr)
	}()
	return read(scope)
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
	cursor, err := a.query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	return cursor.rows, nil
}

func (a *transactionAccess) query(ctx context.Context, statement string, args ...any) (*ownedCursor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.executor == nil {
		return nil, sql.ErrTxDone
	}
	cursor, err := queryCursor(ctx, a.executor, statement, args...)
	if err != nil {
		return nil, err
	}
	a.cursors = append(a.cursors, cursor)
	return cursor, nil
}

func queryCursor(ctx context.Context, executor sqlExecutor, statement string, args ...any) (*ownedCursor, error) {
	release := &cursorRelease{done: make(chan struct{})}
	child, cancel := context.WithCancel(context.WithValue(ctx, cursorReleaseKey{}, release))
	rows, err := executor.QueryContext(child, statement, args...)
	if err != nil {
		cancel()
		return nil, err
	}
	return &ownedCursor{rows: rows, cancel: cancel, release: release}, nil
}

func (a *transactionAccess) QueryRowContext(ctx context.Context, statement string, args ...any) *Row {
	cursor, err := a.query(ctx, statement, args...)
	return &Row{cursor: cursor, err: err, scopeDone: a.done}
}

// The narrow view excludes write and pool-lifetime authority even through a type
// assertion. The connection authorizer also rejects writes expressed as queries.
type readAccess struct{ access Reader }

func (r readAccess) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	return r.access.QueryContext(ctx, statement, args...)
}

func (r readAccess) QueryRowContext(ctx context.Context, statement string, args ...any) *Row {
	return r.access.QueryRowContext(ctx, statement, args...)
}

type databaseAccess struct{ sqlExecutor }

func (a databaseAccess) QueryRowContext(ctx context.Context, statement string, args ...any) *Row {
	cursor, err := queryCursor(ctx, a.sqlExecutor, statement, args...)
	return &Row{cursor: cursor, err: err}
}
