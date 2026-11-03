package sqlite

import (
	"database/sql"
	"errors"
)

// recordOnce stores an idempotent fact. read reports whether an existing row
// equals the new one: an equal row accepts the retry without writing, a
// different row conflicts, and only a missing row is inserted.
func recordOnce(read func() (same bool, err error), insert func() error) error {
	same, err := read()
	switch {
	case err == nil && same:
		return nil
	case err == nil:
		return ErrConflict
	case !errors.Is(err, sql.ErrNoRows):
		return err
	default:
		return insert()
	}
}
