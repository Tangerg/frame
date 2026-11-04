package sqlite

import (
	"context"
	"errors"

	"github.com/Tangerg/frame/value"
)

type protectedRecord struct {
	id             string
	request, proof []byte
	state          string
}

func (r *protectedRecord) scan(row interface{ Scan(...any) error }) error {
	var digest value.Digest
	if err := row.Scan(&r.id, &r.request, &digest, &r.state, &r.proof); err != nil {
		return err
	}
	if value.Hash(r.request) != digest {
		return errors.New("sqlite: execution protection integrity failure")
	}
	return nil
}

func readProtectionRow(ctx context.Context, tx Reader, query, id string) (protectedRecord, error) {
	var row protectedRecord
	err := row.scan(tx.QueryRowContext(ctx, query, id))
	return row, err
}

func readRecord[T any](ctx context.Context, store *Store, query string, decode func(protectedRecord) (T, error), id string) (T, error) {
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

func readRecords[T any](ctx context.Context, store *Store, query string, decode func(protectedRecord) (T, error), args ...any) ([]T, error) {
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
