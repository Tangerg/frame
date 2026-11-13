package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Tangerg/frame"
)

// The connection stays exclusively owned until COMMIT or rollback has settled.
// Cancellation never abandons a transaction that can commit after admission opens.
type transactionMode uint8

const (
	writeTransaction transactionMode = iota
	readTransaction
)

type transactionPhase uint8

const (
	transactionBeginning transactionPhase = iota
	transactionActive
	transactionCommitting
	transactionSettled
)

// beginStatement takes the write lock up front for a write transaction, so
// it can never fail to upgrade after reading.
func (m transactionMode) beginStatement() string {
	if m == readTransaction {
		return "BEGIN"
	}
	return "BEGIN IMMEDIATE"
}

// A failed BEGIN acknowledgement cannot prove whether a transaction exists.
// Disposing its original connection settles either case without inventing a
// rollback failure. Later phases retain their rollback and commit evidence.
func (s *databaseOwner) settleFailedTransaction(connection *sql.Conn, scope *connectionScope, phase transactionPhase, err error, unresolvedCommit func(error)) (bool, error) {
	var rollbackErr error
	if phase != transactionBeginning {
		_, rollbackErr = connection.ExecContext(context.Background(), `ROLLBACK`)
		if rollbackErr == nil && scope.failure() == nil {
			return true, err
		}
		if rollbackErr != nil && phase == transactionCommitting {
			err = errors.Join(err, errCommitUnresolved)
			if unresolvedCommit != nil {
				unresolvedCommit(errors.Join(err, rollbackErr))
			}
		}
	}
	returnable := true
	if released, closeErr := closeOriginalConnection(connection); !released {
		returnable = false
		s.retainUnconfirmedConnection(connection, errors.Join(rollbackErr, closeErr))
		err = errors.Join(err, ErrIndeterminate, closeErr)
	}
	return returnable, errors.Join(err, rollbackErr)
}

func (s *databaseOwner) transaction(ctx context.Context, apply func(Executor) error) error {
	return s.transact(ctx, writeTransaction, apply, nil)
}
func (s *databaseOwner) transact(ctx context.Context, mode transactionMode, apply func(Executor) error, unresolvedCommit func(error)) (err error) {
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	if err := s.admitTransaction(); err != nil {
		return err
	}
	defer s.transactions.Done()
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	returnConnection := true
	defer func() {
		if !returnConnection {
			return
		}
		if closeErr := connection.Close(); !errors.Is(closeErr, sql.ErrConnDone) {
			err = errors.Join(err, closeErr)
		}
	}()
	// A queued transaction may have been admitted before the original failed.
	// Normal Close joins that work; an unconfirmed outcome refuses its callback.
	if err := s.transactionFailure(); err != nil {
		return err
	}
	phase := transactionBeginning
	scope := newConnectionScope(connection, connection)
	defer func() {
		recovered := recover()
		scope.seal()
		if phase != transactionSettled {
			returnConnection, err = s.settleFailedTransaction(connection, scope, phase, err, unresolvedCommit)
			err = scope.include(errors.Join(err, frame.ContextError(ctx)))
		}
		if recovered != nil {
			if phase != transactionSettled && err != nil {
				panic(errors.Join(panicError(recovered), err))
			}
			panic(recovered)
		}
	}()
	// A failed acknowledgement can follow a successful BEGIN. This owner must
	// settle or close that same connection before it can return to the pool.
	if err := beginTransaction(ctx, connection, mode); err != nil {
		return errors.Join(err, frame.ContextError(ctx))
	}
	phase = transactionActive
	if err := scope.enter(); err != nil {
		return err
	}
	access := newTransactionAccess(scope)
	body := func() error { return access.run(func() error { return apply(access) }) }
	if mode == readTransaction {
		err = access.read(ctx, func(*connectionScope) error { return body() })
	} else {
		err = body()
	}
	scope.seal()
	err = scope.include(errors.Join(err, frame.ContextError(ctx)))
	if err != nil {
		return err
	}
	if err = frame.ContextError(ctx); err != nil {
		return err
	}
	phase = transactionCommitting
	if _, err = connection.ExecContext(context.Background(), `COMMIT`); err != nil {
		return err
	}
	phase = transactionSettled
	return nil
}
