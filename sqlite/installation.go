package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"

	"github.com/Tangerg/frame"
)

var _ frame.InstallationStore = (*Store)(nil)

func (s *Store) CommitInstallation(ctx context.Context, change frame.InstallationChange) (state frame.CommitState, err error) {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return frame.CommitRejected, err
	}
	id, err := changedInstallation(change)
	if err != nil {
		return frame.CommitRejected, err
	}
	request, err := json.Marshal(change)
	if err != nil {
		return frame.CommitRejected, err
	}
	submission, err := s.control.prepare(ctx, request)
	if err != nil {
		if errors.Is(err, ErrIndeterminate) {
			return frame.CommitIndeterminate, err
		}
		return frame.CommitRejected, err
	}
	defer func() {
		cause := recover()
		if cause == nil && state != frame.CommitRejected {
			return
		}
		// The private control receipt has no external holder to release it after
		// rejection or panic. Compaction still requires terminal synchronization.
		cleanupErr := s.control.compactSettled(context.WithoutCancel(ctx))
		if cause != nil {
			if cleanupErr != nil {
				panic(errors.Join(panicError(cause), cleanupErr))
			}
			panic(cause)
		}
		err = errors.Join(err, cleanupErr)
	}()
	settlement, err := s.control.commit(ctx, submission, request, func(ctx context.Context, tx Executor) ([]byte, error) {
		return []byte(id), s.applyInstallation(ctx, tx, id, change)
	}, nil, confirmByCaller)
	return settlement.State, err
}

// changedInstallation names the one installation a change replaces.
func changedInstallation(change frame.InstallationChange) (frame.InstallationID, error) {
	var id frame.InstallationID
	if change.Before != nil {
		id = change.Before.ID
	}
	if change.After != nil {
		if id != "" && id != change.After.ID {
			return "", ErrConflict
		}
		id = change.After.ID
	}
	return id, id.Validate()
}

// applyInstallation compares the stored record with the expected one and
// replaces it, then projects the same change into the business tables.
func (s *Store) applyInstallation(ctx context.Context, tx Executor, id frame.InstallationID, change frame.InstallationChange) error {
	if err := checkInstalled(ctx, tx, id, change.Before); err != nil {
		return err
	}
	if change.After == nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM frame_installations WHERE id=?`, id); err != nil {
			return err
		}
	} else {
		encoded, err := json.Marshal(change.After)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO frame_installations(id,record) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET record=excluded.record`, id, encoded); err != nil {
			return err
		}
	}
	if s.projectInstallation == nil {
		return nil
	}
	return s.projectInstallation(ctx, tx, cloneChange(change))
}

func checkInstalled(ctx context.Context, tx Executor, id frame.InstallationID, expected *frame.InstallationRecord) error {
	var stored []byte
	err := tx.QueryRowContext(ctx, `SELECT record FROM frame_installations WHERE id=?`, id).Scan(&stored)
	switch {
	case expected == nil && err == nil:
		return ErrConflict
	case expected == nil && errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	encoded, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, encoded) {
		return ErrConflict
	}
	return nil
}

func cloneChange(change frame.InstallationChange) frame.InstallationChange {
	var projection frame.InstallationChange
	if change.Before != nil {
		before := change.Before.Clone()
		projection.Before = &before
	}
	if change.After != nil {
		after := change.After.Clone()
		projection.After = &after
	}
	return projection
}

func (s *Store) ConfirmInstallation(ctx context.Context, id frame.InstallationID) error {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return err
	}
	if err := s.transaction(ctx, func(tx Executor) error {
		rows, err := tx.QueryContext(ctx, `SELECT sequence FROM frame_submissions WHERE owner_id=? AND state=? AND synchronized=0 AND evidence=? ORDER BY sequence`, s.control.id, submissionCommitted, []byte(id))
		if err != nil {
			return err
		}
		var sequences []uint64
		for rows.Next() {
			var sequence uint64
			if err := rows.Scan(&sequence); err != nil {
				_ = rows.Close()
				return err
			}
			sequences = append(sequences, sequence)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(sequences) != 1 {
			return ErrUnsettled
		}
		_, err = tx.ExecContext(ctx, `UPDATE frame_submissions SET synchronized=1 WHERE owner_id=? AND sequence=?`, s.control.id, sequences[0])
		return err
	}); err != nil {
		return err
	}
	return s.control.compactSettled(ctx)
}

func (s *Store) Installations(ctx context.Context) (records []frame.InstallationRecord, err error) {
	err = s.transaction(ctx, func(tx Executor) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,record FROM frame_installations ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id frame.InstallationID
			var data []byte
			if err := rows.Scan(&id, &data); err != nil {
				return err
			}
			var record frame.InstallationRecord
			if err := jsonv2.Unmarshal(data, &record, jsonv2.RejectUnknownMembers(true)); err != nil {
				return err
			}
			if record.ID != id || id.Validate() != nil || record.Release.Validate() != nil {
				return errors.New("sqlite: invalid installation record")
			}
			for _, unit := range record.Units {
				if err := unit.Validate(); err != nil {
					return err
				}
			}
			records = append(records, record)
		}
		return rows.Err()
	})
	return records, err
}

// RecoverInstallations settles old control intents after exclusive ownership is
// acquired. The Host must then restore the returned records, confirm them and
// restore every other owner before it clears RecoveryRequired.
func (s *Store) RecoverInstallations(ctx context.Context) ([]frame.InstallationRecord, error) {
	if err := s.control.RecoverPrepared(ctx); err != nil {
		return nil, err
	}
	return s.Installations(ctx)
}

func (s *Store) ConfirmRestoredInstallations(ctx context.Context) error {
	return s.admission.Maintenance(ctx, func() error {
		if err := s.transaction(ctx, func(tx Executor) error {
			_, err := tx.ExecContext(ctx, `UPDATE frame_submissions SET synchronized=1 WHERE owner_id=? AND state!=?`, s.control.id, submissionPrepared)
			return err
		}); err != nil {
			return err
		}
		return s.control.compactSettled(ctx)
	})
}
