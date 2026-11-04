package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/Tangerg/frame/internal/cancellation"

	modernc "modernc.org/sqlite"
)

var errStatementPolicyUnconfirmed = errors.New("sqlite: connection statement policy could not be restored")

// The authorizer belongs to the physical connection. Overlapping readers share
// its restriction until their last release; settlement seals and joins them
// before restoring transaction control to the original owner.
type connectionScope struct {
	connection  *sql.Conn
	executor    sqlExecutor
	mu          sync.Mutex
	readers     int
	readersDone chan struct{}
	sealed      bool
	active      bool
	unconfirmed error
}

func newConnectionScope(connection *sql.Conn, executor sqlExecutor) *connectionScope {
	return &connectionScope{connection: connection, executor: executor}
}

func (s *connectionScope) authorize(policy modernc.AuthorizerFn) error {
	return s.connection.Raw(func(connection any) error {
		registerer, ok := connection.(*cursorConnection)
		if !ok {
			return errors.New("sqlite: transactions require a database opened by OpenSchema")
		}
		return registerer.RegisterAuthorizer(policy)
	})
}

func (s *connectionScope) enter() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A failed acknowledgement can follow successful registration. Cleanup must
	// remove that same policy even when no callback was admitted.
	s.active = true
	return s.authorize(transactionStatements.authorize)
}

func (s *connectionScope) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unconfirmed
}

func (s *connectionScope) requireDiscard(cause error) {
	s.mu.Lock()
	s.unconfirmed = errors.Join(s.unconfirmed, cause)
	s.mu.Unlock()
}

func (s *connectionScope) include(err error) error {
	failure := s.failure()
	if errors.Is(err, failure) {
		return err
	}
	return errors.Join(err, failure)
}

func (s *connectionScope) restoreLocked(policy modernc.AuthorizerFn) error {
	if err := s.authorize(policy); err != nil {
		s.unconfirmed = errors.Join(s.unconfirmed, errStatementPolicyUnconfirmed, err)
	}
	return s.unconfirmed
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
	if err := cancellation.Err(ctx); err != nil {
		return err
	}
	if s.readers != 0 {
		s.readers++
		return nil
	}
	if err := s.authorize(readStatements.authorize); err != nil {
		return errors.Join(err, s.restoreLocked(transactionStatements.authorize))
	}
	s.readers = 1
	s.readersDone = make(chan struct{})
	return nil
}

func (s *connectionScope) releaseRead() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readers--
	if s.readers != 0 {
		return nil
	}
	err := s.restoreLocked(transactionStatements.authorize)
	close(s.readersDone)
	s.readersDone = nil
	return err
}

func (s *connectionScope) seal() {
	s.mu.Lock()
	s.sealed = true
	done := s.readersDone
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		s.active = false
		_ = s.restoreLocked(nil)
	}
}
