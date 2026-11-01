// Package sqlite owns strict SQLite schema admission and transaction lifetimes.
// Business schemas and records remain with their calling owners.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const busyTimeout = 5 * time.Second

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
	wait, cancel := context.WithTimeout(ctx, busyTimeout)
	defer cancel()
	for {
		var mode string
		err := connection.QueryRowContext(wait, `PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil {
			if mode != "wal" {
				return fmt.Errorf("journal mode is %q, require wal", mode)
			}
			_, err = connection.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeout/time.Millisecond))
			return err
		}
		var busy *sqlitedriver.Error
		if !errors.As(err, &busy) || busy.Code() != sqlite3.SQLITE_BUSY {
			return err
		}
		// WAL conversion can bypass SQLite's busy handler to avoid a lock-upgrade
		// deadlock. Wait only for this idempotent configuration, before owner SQL;
		// WAL persists, so replacement pool connections need no mode transition.
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
			return wait.Err()
		case <-timer.C:
		}
	}
}

func InstallSchema(ctx context.Context, db *sql.DB, stmts []string) error {
	var current bool
	err := runIndependentTx(ctx, db, &sql.TxOptions{ReadOnly: true}, func(ctx context.Context) error {
		var err error
		current, err = requireCurrentSchema(ctx, Connection(ctx, db), stmts)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: inspect current schema: %w", err)
	}
	// Reexecuting initialization on an existing schema competes with recovery
	// writes and can recreate missing owner facts instead of exposing corruption.
	if current {
		return nil
	}
	err = RunIndependentTx(ctx, db, func(ctx context.Context) error {
		tx := Connection(ctx, db)
		current, err := requireCurrentSchema(ctx, tx, stmts)
		if err != nil {
			return err
		}
		if current {
			return nil
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("sqlite: install current schema: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlite: install current schema: %w", err)
	}
	return nil
}

// requireCurrentSchema admits a data directory that is empty or exactly
// current: every table, index and trigger the current statements define, with
// their definitions, and nothing else. The statements would only partly apply
// to any other directory, so it is refused before one of them runs; there is
// no migration path, and a former shape is never repaired in place.
func requireCurrentSchema(ctx context.Context, tx Executor, stmts []string) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name, sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		return false, fmt.Errorf("sqlite: inspect schema: %w", err)
	}
	stored := map[string]string{}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("sqlite: inspect schema: %w", err)
		}
		if _, present := stored[name]; present {
			return false, errors.Join(fmt.Errorf("sqlite: multiple schema objects named %s", name), rows.Close())
		}
		stored[name] = definition
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, fmt.Errorf("sqlite: inspect schema: %w", err)
	}
	if len(stored) == 0 {
		return false, nil
	}
	for _, stmt := range stmts {
		if !strings.HasPrefix(stmt, "CREATE ") {
			continue
		}
		name, definition, err := schemaObject(stmt)
		if err != nil {
			return false, err
		}
		if stored[name] != definition {
			return false, fmt.Errorf("sqlite: incompatible %s schema; open a fresh data directory", name)
		}
		delete(stored, name)
	}
	for _, name := range slices.Sorted(maps.Keys(stored)) {
		return false, fmt.Errorf("sqlite: incompatible schema object %s; open a fresh data directory", name)
	}
	return true, nil
}

// schemaObject names the object a current statement creates and the definition
// SQLite records for it, which omits the IF NOT EXISTS clause.
func schemaObject(stmt string) (name, definition string, err error) {
	const clause = " IF NOT EXISTS "
	before, after, found := strings.Cut(stmt, clause)
	if !found {
		return "", "", fmt.Errorf("sqlite: schema statement lacks %q: %.40s", strings.TrimSpace(clause), stmt)
	}
	end := strings.IndexFunc(after, func(r rune) bool { return unicode.IsSpace(r) || r == '(' })
	if end <= 0 {
		return "", "", fmt.Errorf("sqlite: schema statement names no object: %.40s", stmt)
	}
	return after[:end], before + " " + after, nil
}
