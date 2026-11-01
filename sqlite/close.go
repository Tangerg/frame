package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// Close joins the original Store shutdown. An unconfirmed connection remains
// owned, and its authority lock remains held, until that exact connection closes.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.startClose()
	result := make(chan error, 1)
	select {
	case <-s.closeDone:
		return s.closeErr
	case s.closeRequests <- result:
	}
	select {
	case err := <-result:
		return err
	case <-s.closeDone:
		return s.closeErr
	}
}

func (s *Store) startClose() {
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closing = true
		s.lifecycleMu.Unlock()
		s.closeRequests = make(chan chan error)
		s.closeDone = make(chan struct{})
		go s.closeOriginal()
	})
}

// This is the outstanding resource owner, including when Open's caller drops
// an error. It keeps the original connection and os.File alive while release is
// unconfirmed; no finalizer can silently relinquish the authority in that state.
func (s *Store) closeOriginal() {
	for result := range s.closeRequests {
		err := s.advanceClose()
		if err == nil || s.lock == nil {
			s.closeErr = err
			close(s.closeDone)
			result <- err
			return
		}
		s.admission.RequireRecovery(err)
		result <- err
	}
}

func (s *Store) advanceClose() error {
	s.transactions.Wait()
	if s.db != nil && !s.databaseSealed {
		// database/sql.Close only disposes idle connections. Pin the sole
		// original connection first, seal the pool, then observe its own close.
		if len(s.unconfirmed) == 0 && s.db.Stats().OpenConnections != 0 {
			connection, err := s.db.Conn(context.Background())
			if err != nil {
				return err
			}
			s.unconfirmed = append(s.unconfirmed, connection)
		}
		s.databaseCloseErr = s.db.Close()
		s.databaseSealed = true
	}
	remaining := s.unconfirmed[:0]
	closeErr := s.databaseCloseErr
	for _, connection := range s.unconfirmed {
		released, err := closeOriginalConnection(connection)
		closeErr = errors.Join(closeErr, err)
		if !released {
			remaining = append(remaining, connection)
		}
	}
	clear(s.unconfirmed[len(remaining):])
	s.unconfirmed = remaining
	if len(remaining) != 0 || s.databaseCloseErr != nil {
		return errors.Join(ErrIndeterminate, closeErr)
	}
	if s.lock != nil {
		closeErr = errors.Join(closeErr, releaseAuthority(s.lock))
		s.lock = nil
	}
	s.unconfirmed = nil
	return closeErr
}

func (s *Store) retainUnconfirmedConnection(connection *sql.Conn, cause error) {
	s.lifecycleMu.Lock()
	s.unconfirmed = append(s.unconfirmed, connection)
	s.lifecycleMu.Unlock()
	s.admission.RequireRecovery(cause)
	s.startClose()
	// The checked-out original remains available to its close owner. Sealing
	// the pool prevents queued transactions from waiting forever or replacing it.
	s.databaseCloseErr = s.db.Close()
	s.databaseSealed = true
}

func closeOriginalConnection(connection *sql.Conn) (bool, error) {
	released := false
	err := connection.Raw(func(raw any) error {
		// This adapter owns modernc SQLite connections. Its Close retains the
		// same sqlite3 handle on close_v2 failure and clears it only on success;
		// a later attempt therefore observes that original resource's release.
		if err := raw.(driver.Conn).Close(); err != nil {
			return err
		}
		released = true
		return driver.ErrBadConn
	})
	if !released {
		return false, err
	}
	if err := connection.Close(); !errors.Is(err, sql.ErrConnDone) {
		return true, err
	}
	return true, nil
}
