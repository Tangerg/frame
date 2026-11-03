package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

const realmReceiptSchema = `CREATE TABLE frame_realm_receipts(operation_id TEXT PRIMARY KEY,request_digest TEXT NOT NULL,reserved_bytes INTEGER NOT NULL CHECK(reserved_bytes>0),receipt BLOB,history_bytes INTEGER GENERATED ALWAYS AS (length(CAST(operation_id AS BLOB))+length(CAST(request_digest AS BLOB))+CASE WHEN receipt IS NULL THEN reserved_bytes ELSE length(receipt) END) VIRTUAL,CHECK(receipt IS NULL OR length(receipt)<=reserved_bytes));`

// A reservation admits the largest possible reply before an operation can act.
// The row owns both pending capacity and the eventual reply, so completion never
// competes with another operation for space it was already promised.
type ReceiptReservation struct {
	store *Store
	id    string
}

func (s *Store) ReserveRealmReceipt(ctx context.Context, id string, request value.Digest, maximumBytes int64) (*ReceiptReservation, error) {
	if !frame.ValidName(id) || request.Validate() != nil || maximumBytes < 1 {
		return nil, frame.ErrInvalid
	}
	if maximumBytes > s.maxRealmHistoryBytes-int64(len(id)+len(request.String())) {
		return nil, frame.ErrCapacity
	}
	err := s.transaction(ctx, func(tx Executor) error {
		var reserved int64
		var original value.Digest
		err := tx.QueryRowContext(ctx, `SELECT request_digest,reserved_bytes FROM frame_realm_receipts WHERE operation_id=?`, id).Scan(&original, &reserved)
		if err == nil {
			if original != request || reserved != maximumBytes {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO frame_realm_receipts(operation_id,request_digest,reserved_bytes) VALUES(?,?,?)`, id, request, maximumBytes); err != nil {
			return err
		}
		return s.checkRealmHistoryCapacity(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return &ReceiptReservation{store: s, id: id}, nil
}

func (s *Store) checkRealmHistoryCapacity(ctx context.Context, tx Reader) error {
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT sum(history_bytes) FROM frame_realms),0)+coalesce((SELECT sum(history_bytes) FROM frame_realm_receipts),0)`).Scan(&used); err != nil {
		return err
	}
	if used > s.maxRealmHistoryBytes {
		return frame.ErrCapacity
	}
	return nil
}

// Reconstruction admits only cleanup for old Realms. Their execution and exit
// evidence remain with their original owners when these epoch-local replies go.
func (s *Store) RealmReceipt(ctx context.Context, id string) ([]byte, error) {
	if !frame.ValidName(id) {
		return nil, frame.ErrInvalid
	}
	var data []byte
	err := s.transaction(ctx, func(tx Executor) error {
		return tx.QueryRowContext(ctx, `SELECT receipt FROM frame_realm_receipts WHERE operation_id=? AND receipt IS NOT NULL`, id).Scan(&data)
	})
	return data, err
}

func (r *ReceiptReservation) Record(ctx context.Context, data []byte) error {
	if r == nil || r.store == nil || len(data) == 0 {
		return frame.ErrInvalid
	}
	return r.store.transaction(ctx, func(tx Executor) error {
		var prior []byte
		var reserved int64
		if err := tx.QueryRowContext(ctx, `SELECT reserved_bytes,receipt FROM frame_realm_receipts WHERE operation_id=?`, r.id).Scan(&reserved, &prior); err != nil {
			return err
		}
		if prior != nil {
			if !bytes.Equal(data, prior) {
				return ErrConflict
			}
			return nil
		}
		if int64(len(data)) > reserved {
			return frame.ErrCapacity
		}
		_, err := tx.ExecContext(ctx, `UPDATE frame_realm_receipts SET receipt=? WHERE operation_id=?`, data, r.id)
		return err
	})
}
