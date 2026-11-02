package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

var errReadModeUnconfirmed = errors.New("sqlite: connection read mode could not be restored")

type queryMode uint8

const (
	queryReadWrite queryMode = iota
	queryOnly
)

// query_only belongs to the connection, not to one callback. Overlapping and
// nested readers share the original mode and its restoration obligation; only
// their last release may restore it. Transaction settlement seals and joins them.
type connectionScope struct {
	executor     Executor
	mu           sync.Mutex
	readers      int
	originalMode queryMode
	readersDone  chan struct{}
	sealed       bool
	unconfirmed  error
}

func newConnectionScope(executor Executor) *connectionScope {
	return &connectionScope{executor: executor}
}

func (s *connectionScope) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unconfirmed
}

func (s *connectionScope) include(err error) error {
	if errors.Is(err, errReadModeUnconfirmed) {
		return err
	}
	return errors.Join(err, s.failure())
}

func (s *connectionScope) acquireRead(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return sql.ErrTxDone
	}
	if s.unconfirmed != nil {
		return s.unconfirmed
	}
	if s.readers != 0 {
		s.readers++
		return nil
	}
	if err := s.executor.QueryRowContext(ctx, "PRAGMA query_only").Scan(&s.originalMode); err != nil {
		return err
	}
	switch s.originalMode {
	case queryReadWrite:
		// An unacknowledged ON may already have executed. Restoration belongs
		// to this same owner before any callback is admitted.
		if _, err := s.executor.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
			return errors.Join(err, s.restoreLocked(ctx))
		}
	case queryOnly:
	default:
		s.unconfirmed = errors.Join(errReadModeUnconfirmed, errors.New("sqlite: invalid query-only mode"))
		return s.unconfirmed
	}
	s.readers = 1
	s.readersDone = make(chan struct{})
	return nil
}

func (s *connectionScope) restoreLocked(ctx context.Context) error {
	if s.originalMode != queryReadWrite {
		return nil
	}
	if _, err := s.executor.ExecContext(context.WithoutCancel(ctx), "PRAGMA query_only=OFF"); err != nil {
		s.unconfirmed = errors.Join(errReadModeUnconfirmed, err)
	}
	return s.unconfirmed
}

func (s *connectionScope) releaseRead(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readers--
	if s.readers != 0 {
		return nil
	}
	err := s.restoreLocked(ctx)
	close(s.readersDone)
	s.readersDone = nil
	return err
}

func (s *connectionScope) read(ctx context.Context, read func() error) (err error) {
	if err := s.acquireRead(ctx); err != nil {
		return err
	}
	defer func() {
		recovered := recover()
		releaseErr := s.releaseRead(ctx)
		if recovered != nil {
			if releaseErr != nil {
				panic(errors.Join(panicError(recovered), releaseErr))
			}
			panic(recovered)
		}
		err = errors.Join(err, releaseErr)
	}()
	return read()
}

func (s *connectionScope) seal() {
	s.mu.Lock()
	s.sealed = true
	done := s.readersDone
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}
