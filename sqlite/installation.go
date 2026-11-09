package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"maps"
	"slices"

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
	commit := installationCommit{owner: s.control, id: submission, request: request}
	defer func() {
		cause := recover()
		var cleanupErr error
		if state != frame.CommitSucceeded {
			state, cleanupErr = commit.conclude(context.WithoutCancel(ctx))
			err = errors.Join(err, cleanupErr)
		}
		if cause != nil {
			if cleanupErr != nil {
				panic(errors.Join(panicError(cause), cleanupErr))
			}
			panic(cause)
		}
	}()
	settlement, err := s.control.commit(ctx, submission, request, func(ctx context.Context, tx Executor) ([]byte, error) {
		return []byte(id), s.applyInstallation(ctx, tx, id, change)
	}, nil, confirmByCaller)
	return settlement.State, err
}

// The installation call owns its private intent even before mutation enters.
// Every unsuccessful return, panic and Goexit must conclude that same intent.
type installationCommit struct {
	owner   Owner
	id      SubmissionID
	request []byte
}

func (c installationCommit) conclude(ctx context.Context) (frame.CommitState, error) {
	settlement, err := c.owner.settlePrepared(ctx, c.id, c.request)
	if err != nil || settlement.State != frame.CommitRejected {
		err = errors.Join(ErrIndeterminate, ErrUnsettled, err)
		c.owner.store.admission.RequireRecovery(err)
		return frame.CommitIndeterminate, err
	}
	if _, err := c.owner.synchronize(settlement, nil, confirmOnCommit); err != nil {
		return frame.CommitRejected, err
	}
	return frame.CommitRejected, c.owner.compactSettled(ctx)
}

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
	for _, ownerID := range slices.Sorted(maps.Keys(s.owners)) {
		owner := s.owners[ownerID]
		if owner.projectInstallation == nil {
			continue
		}
		if err := owner.withinSchema(tx, func(grant Executor) error {
			return owner.projectInstallation(ctx, grant, cloneChange(change))
		}); err != nil {
			return err
		}
	}
	return nil
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
		if _, err := s.control.readSubmissions(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT sequence FROM frame_submissions WHERE owner_id=? AND state=? AND evidence=? ORDER BY sequence`, s.control.id, submissionCommitted, []byte(id))
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
		return s.control.confirmIn(ctx, tx, SubmissionID{Store: s.id, Owner: s.control.id, Sequence: sequences[0]})
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
			settlements, err := s.control.readSubmissions(ctx, tx)
			if err != nil {
				return err
			}
			for _, settlement := range settlements {
				if settlement.State != frame.CommitIndeterminate {
					if err := s.control.confirmIn(ctx, tx, settlement.ID); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		return s.control.compactSettled(ctx)
	})
}
