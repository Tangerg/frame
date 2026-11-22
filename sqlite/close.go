package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// Close joins the original database shutdown. An unconfirmed connection remains
// owned, and its authority lock remains held, until that exact connection closes.
func (d *databaseOwner) close() error {
	if d == nil {
		return nil
	}
	d.startClose()
	result := make(chan error, 1)
	select {
	case <-d.closeDone:
		return d.closeErr
	case d.closeRequests <- result:
	}
	select {
	case err := <-result:
		return err
	case <-d.closeDone:
		return d.closeErr
	}
}

func (d *databaseOwner) startClose() {
	d.closeOnce.Do(func() {
		d.lifecycleMu.Lock()
		d.closing = true
		d.lifecycleMu.Unlock()
		d.closeRequests = make(chan chan error)
		d.closeDone = make(chan struct{})
		go d.closeOriginal()
	})
}

// This is the outstanding resource owner, including when Open's caller drops
// an error. It keeps the original connection and os.File alive while release is
// unconfirmed; no finalizer can silently relinquish the authority in that state.
func (d *databaseOwner) closeOriginal() {
	for result := range d.closeRequests {
		err := d.advanceClose()
		if err == nil || len(d.unconfirmed) == 0 && d.databaseCloseErr == nil && d.lock == nil {
			d.closeErr = err
			close(d.closeDone)
			result <- err
			return
		}
		d.requireRecovery(err)
		result <- err
	}
}

func (d *databaseOwner) advanceClose() error {
	d.transactions.Wait()
	if d.db != nil {
		if err := d.sealDatabase(); err != nil {
			return err
		}
	}
	d.lifecycleMu.Lock()
	sealErr := d.databaseCloseErr
	d.lifecycleMu.Unlock()
	remaining := d.unconfirmed[:0]
	closeErr := sealErr
	for _, connection := range d.unconfirmed {
		released, err := closeOriginalConnection(connection)
		closeErr = errors.Join(closeErr, err)
		if !released {
			remaining = append(remaining, connection)
		}
	}
	clear(d.unconfirmed[len(remaining):])
	d.unconfirmed = remaining
	if len(remaining) != 0 || sealErr != nil {
		return errors.Join(ErrIndeterminate, closeErr)
	}
	if d.lock != nil {
		closeErr = errors.Join(closeErr, releaseAuthority(d.lock))
		d.lock = nil
	}
	d.unconfirmed = nil
	return closeErr
}

func (d *databaseOwner) retainUnconfirmedConnection(connection *sql.Conn, cause error) {
	d.lifecycleMu.Lock()
	d.unconfirmed = append(d.unconfirmed, connection)
	d.lifecycleMu.Unlock()
	d.requireRecovery(cause)
	d.startClose()
	// The checked-out original remains available to its close owner. Sealing
	// the pool prevents queued transactions from waiting forever or replacing it.
	_ = d.sealDatabase()
}

// sealDatabase closes the pool once. database/sql.Close only disposes idle
// connections, so the sole open connection is pinned first, unless one is
// already retained, and its own close is observed afterwards.
func (d *databaseOwner) sealDatabase() error {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.databaseSealed {
		return nil
	}
	if len(d.unconfirmed) == 0 && d.db.Stats().OpenConnections != 0 {
		connection, err := d.db.Conn(context.Background())
		if err != nil {
			return err
		}
		d.unconfirmed = append(d.unconfirmed, connection)
	}
	d.databaseCloseErr = d.db.Close()
	d.databaseSealed = true
	return nil
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
