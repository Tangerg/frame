// Package sqlite owns strict SQLite schema admission and transaction lifetimes.
// Business schemas and records remain with their calling owners.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	busyTimeout       = 5 * time.Second
	busyRetryInterval = time.Millisecond
	primaryResultMask = 0xff
)

// OpenSchema admits an exact owner-supplied schema in its private database.
func OpenSchema(ctx context.Context, path string, statements []string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_txlock=immediate&_pragma=foreign_keys(ON)&_pragma=busy_timeout(%d)",
		url.PathEscape(filepath.ToSlash(path)), busyTimeout/time.Millisecond,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	// Acquire the write slot before reading a mutable snapshot: a deferred
	// upgrade can fail immediately even when the busy timeout allows waiting.
	// One connection serializes this owner's write-sets; SQLite coordinates peers.
	db.SetMaxOpenConns(1)

	if path != ":memory:" {
		if err := admitWAL(ctx, db); err != nil {
			return nil, errors.Join(fmt.Errorf("sqlite: admit wal: %w", err), cancellationError(ctx), db.Close())
		}
	}
	if err := InstallSchema(ctx, db, statements); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func admitWAL(ctx context.Context, db *sql.DB) (err error) {
	connection, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, connection.Close()) }()
	if _, err := connection.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return err
	}
	err = waitForSQLite(ctx, busyTimeout, func() error {
		var mode string
		if err := connection.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
			return err
		}
		if mode != "wal" {
			return fmt.Errorf("journal mode is %q, require wal", mode)
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = connection.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeout/time.Millisecond))
	return err
}

// SQLite's busy handler may sleep beyond Go cancellation. Retry only lock
// admission with that handler disabled, before any application SQL has run.
func waitForSQLite(ctx context.Context, limit time.Duration, attempt func() error) error {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		if err := cancellationError(ctx); err != nil {
			return err
		}
		err := attempt()
		var busy *sqlitedriver.Error
		// Extended busy result codes share the primary code in the low byte.
		if !errors.As(err, &busy) || busy.Code()&primaryResultMask != sqlite3.SQLITE_BUSY || limit == 0 {
			return err
		}
		timer := time.NewTimer(busyRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return cancellationError(ctx)
		case <-deadline.C:
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

func InstallSchema(ctx context.Context, db *sql.DB, stmts []string) error {
	expected, err := schemaShape(ctx, stmts)
	if err != nil {
		return fmt.Errorf("sqlite: declared schema: %w", err)
	}
	inspect := func(ctx context.Context, tx Executor) (bool, error) {
		shape, err := readSchemaShape(ctx, tx)
		if err != nil {
			return false, err
		}
		if shape.IsZero() {
			return false, nil
		}
		if shape != expected {
			return false, errors.New("sqlite: incompatible database schema; open a fresh data directory")
		}
		return true, nil
	}
	var current bool
	err = runIndependentTx(ctx, db, &sql.TxOptions{ReadOnly: true}, func(ctx context.Context) error {
		var err error
		current, err = inspect(ctx, Connection(ctx, db))
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: inspect current schema: %w", err)
	}
	if current {
		return nil
	}
	// A competing initializer can commit before this writer acquires its slot.
	// Replaying seed statements would recreate business facts the owner deleted.
	return RunIndependentTx(ctx, db, func(ctx context.Context) error {
		tx := Connection(ctx, db)
		current, err := inspect(ctx, tx)
		if err != nil || current {
			return err
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("sqlite: install current schema: %w", err)
			}
		}
		return nil
	})
}
