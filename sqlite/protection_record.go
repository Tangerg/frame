package sqlite

import (
	"bytes"
	"context"
	"errors"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

var errProtectionIntegrity = errors.New("sqlite: invalid stored execution protection")

func corruptProtection(cause error) error {
	if cause == nil {
		return nil
	}
	return errors.Join(errProtectionIntegrity, cause)
}

type protectedRecord struct {
	id             string
	request, proof []byte
	state          string
}

func (r *protectedRecord) scan(row interface{ Scan(...any) error }) error {
	var digest string
	if err := row.Scan(&r.id, &r.request, &digest, &r.state, &r.proof); err != nil {
		return err
	}
	if value.Hash(r.request).String() != digest {
		return corruptProtection(errors.New("sqlite: execution protection integrity failure"))
	}
	return nil
}

// recordProtection stores one protection request under its identity. A replay
// of the identical original request confirms it; any other stored request under
// that identity is a conflict, and a corrupt one fences the store.
func recordProtection[T any](ctx context.Context, s *storeAuthority, query, id string, request []byte, decode func(protectedRecord) (T, error), insert func(Executor) error) (framehost.CommitState, error) {
	if len(request) > s.maxRequestBytes {
		return framehost.NotCommitted, frame.ErrCapacity
	}
	return s.protectionTransaction(ctx, func(tx Executor) error {
		return recordOnce(func() (bool, error) {
			row, err := readProtectionRow(ctx, tx, query, id)
			if err != nil {
				return false, err
			}
			if _, err := decode(row); err != nil {
				return false, err
			}
			return bytes.Equal(row.request, request), nil
		}, func() error { return insert(tx) })
	})
}

func readProtectionRow(ctx context.Context, tx Reader, query, id string) (protectedRecord, error) {
	var row protectedRecord
	err := row.scan(tx.QueryRowContext(ctx, query, id))
	return row, err
}

func readRecord[T any](ctx context.Context, store *storeAuthority, query string, decode func(protectedRecord) (T, error), id string) (T, error) {
	var result T
	err := store.Read(ctx, func(tx Reader) error {
		row, err := readProtectionRow(ctx, tx, query, id)
		if err != nil {
			return err
		}
		result, err = decode(row)
		return err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return result, nil
}

func readRecords[T any](ctx context.Context, store *storeAuthority, query string, decode func(protectedRecord) (T, error), args ...any) ([]T, error) {
	var records []T
	err := store.Read(ctx, func(tx Reader) error {
		var err error
		records, err = scanRecords(ctx, tx, query, decode, args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func scanRecords[T any](ctx context.Context, tx Reader, query string, decode func(protectedRecord) (T, error), args ...any) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []T
	for rows.Next() {
		var row protectedRecord
		if err := row.scan(rows); err != nil {
			return nil, err
		}
		record, err := decode(row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
