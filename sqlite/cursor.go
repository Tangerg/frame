package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"

	modernc "modernc.org/sqlite"
)

type sqliteConnection interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
	modernc.AuthorizerRegisterer
}

type sqliteRows interface {
	driver.Rows
	driver.RowsColumnTypeDatabaseTypeName
	driver.RowsColumnTypeLength
	driver.RowsColumnTypeNullable
	driver.RowsColumnTypePrecisionScale
	driver.RowsColumnTypeScanType
}

type cursorConnector struct{ driver.Connector }

func openDatabase(dsn string) (*sql.DB, error) {
	connector, err := modernc.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(cursorConnector{Connector: connector}), nil
}

func (c cursorConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	sqlite, ok := connection.(sqliteConnection)
	if !ok {
		return nil, errors.Join(errors.New("sqlite: driver lacks required connection capabilities"), connection.Close())
	}
	return &cursorConnection{sqliteConnection: sqlite}, nil
}

type cursorConnection struct{ sqliteConnection }
type cursorReleaseKey struct{}

func (c *cursorConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.sqliteConnection.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	release, observed := ctx.Value(cursorReleaseKey{}).(*cursorRelease)
	if !observed {
		return rows, nil
	}
	sqlite, ok := rows.(sqliteRows)
	if !ok {
		return nil, errors.Join(errors.New("sqlite: driver lacks required column metadata"), rows.Close())
	}
	return &cursorRows{sqliteRows: sqlite, release: release}, nil
}

// database/sql may close on EOF or cancellation and makes every later Close a
// no-op. Only this physical cursor can preserve its original release result.
type cursorRelease struct {
	once    sync.Once
	done    chan struct{}
	nextErr error
	err     error
}

type cursorRows struct {
	sqliteRows
	release *cursorRelease
}

func (r *cursorRows) Next(values []driver.Value) error {
	err := r.sqliteRows.Next(values)
	r.release.nextErr = err
	return err
}

func (r *cursorRows) Close() error {
	r.release.once.Do(func() {
		r.release.err = r.sqliteRows.Close()
		close(r.release.done)
	})
	return r.release.err
}

func (r *cursorRelease) result() error {
	select {
	case <-r.done:
		// sqlite3_finalize destroys the statement but repeats its last evaluation
		// error. Preserve additional cleanup failures without poisoning a handled
		// Next error or substituting Rows.Err's cancellation for release evidence.
		step, stepFailed := r.nextErr.(*modernc.Error)
		finalize, repeated := r.err.(*modernc.Error)
		if stepFailed && repeated && step.Code() == finalize.Code() {
			return nil
		}
		return r.err
	default:
		return errCursorCloseUnconfirmed
	}
}
