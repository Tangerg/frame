package sqlite

import (
	"database/sql"
	"errors"
)

// An existing identity never permits replacement of its original fact.
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
