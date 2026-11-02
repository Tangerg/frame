package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"github.com/Tangerg/frame"
)

const realmReceiptSchema = `CREATE TABLE frame_realm_receipts(operation_id TEXT PRIMARY KEY,receipt BLOB NOT NULL);`

// These replies deduplicate a live browser authority. Reconstruction admits only
// cleanup for old Realms; their execution and exit evidence have separate owners.
func (s *Store) RealmReceipt(ctx context.Context, id string) ([]byte, error) {
	var data []byte
	err := s.transaction(ctx, func(tx Executor) error {
		return tx.QueryRowContext(ctx, `SELECT receipt FROM frame_realm_receipts WHERE operation_id=?`, id).Scan(&data)
	})
	return data, err
}
func (s *Store) RecordRealmReceipt(ctx context.Context, id string, data []byte) error {
	if !frame.ValidName(id) || len(data) == 0 {
		return frame.ErrInvalid
	}
	return s.transaction(ctx, func(tx Executor) error {
		var prior []byte
		err := tx.QueryRowContext(ctx, `SELECT receipt FROM frame_realm_receipts WHERE operation_id=?`, id).Scan(&prior)
		if err == nil {
			if !bytes.Equal(data, prior) {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO frame_realm_receipts(operation_id,receipt) VALUES(?,?)`, id, data)
		return err
	})
}
