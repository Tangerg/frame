package sqlite

import (
	"database/sql"
	"errors"
)

// Row uses its query's original cursor so a scoped callback can close an
// unscanned result before returning authority over the connection.
type Row struct {
	cursor    *ownedCursor
	err       error
	scopeDone <-chan struct{}
}

func (r *Row) Err() error {
	if r.err != nil {
		return r.err
	}
	select {
	case <-r.scopeDone:
		return sql.ErrTxDone
	default:
	}
	if r.cursor == nil {
		return sql.ErrNoRows
	}
	return r.cursor.rows.Err()
}

func (r *Row) Scan(dest ...any) (err error) {
	if r.cursor != nil {
		defer func() { err = errors.Join(err, r.cursor.close()) }()
	}
	if err := r.Err(); err != nil {
		return err
	}
	for _, value := range dest {
		if _, borrowed := value.(*sql.RawBytes); borrowed {
			return errors.New("sqlite: row scan cannot retain sql.RawBytes after cursor closure")
		}
	}
	if !r.cursor.rows.Next() {
		if err := r.cursor.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return r.cursor.rows.Scan(dest...)
}
