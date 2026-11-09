package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"

	"github.com/Tangerg/frame"
)

// Database owns standalone transactions and the original physical connections.
// Its pool is private so callers cannot discard an unconfirmed release or issue
// cursors outside a transaction's lifetime.
type Database struct{ *databaseOwner }

func (d *Database) Close() error {
	if d == nil {
		return nil
	}
	return d.databaseOwner.close()
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.databaseOwner.close()
}

type databaseOwner struct {
	db               *sql.DB
	lock             *os.File
	onFailure        func(error)
	lifecycleMu      sync.Mutex
	transactions     sync.WaitGroup
	closing          bool
	failure          error
	databaseSealed   bool
	databaseCloseErr error
	unconfirmed      []*sql.Conn
	closeOnce        sync.Once
	closeRequests    chan chan error
	closeDone        chan struct{}
	closeErr         error
}

func (d *databaseOwner) admitTransaction() error {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.closing {
		return errors.Join(frame.ErrClosed, d.failure)
	}
	d.transactions.Add(1)
	return nil
}

func (d *databaseOwner) requireRecovery(cause error) {
	d.lifecycleMu.Lock()
	d.closing = true
	if d.failure == nil {
		d.failure = cause
	}
	d.lifecycleMu.Unlock()
	if d.onFailure != nil {
		d.onFailure(cause)
	}
}

func (d *databaseOwner) transactionFailure() error {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.failure != nil {
		return errors.Join(frame.ErrClosed, d.failure)
	}
	return nil
}

func (d *Database) Read(ctx context.Context, query func(Reader) error) error {
	if query == nil {
		return frame.ErrInvalid
	}
	return RunReadSnapshot(ctx, d, func(ctx context.Context) error {
		return query(readAccess{access: Connection(ctx, d)})
	})
}
