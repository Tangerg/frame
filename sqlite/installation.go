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

	framehost "github.com/Tangerg/frame/host"
)

var _ framehost.InstallationStore = Store{}

func (s *storeAuthority) CommitInstallation(ctx context.Context, change framehost.InstallationChange) (state framehost.CommitState, err error) {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return framehost.CommitRejected, err
	}
	id, err := changedInstallation(change)
	if err != nil {
		return framehost.CommitRejected, err
	}
	request, err := json.Marshal(change)
	if err != nil {
		return framehost.CommitRejected, err
	}
	submission, err := s.control.prepare(ctx, request)
	if err != nil {
		if errors.Is(err, ErrIndeterminate) {
			return framehost.CommitIndeterminate, err
		}
		return framehost.CommitRejected, err
	}
	commit := installationCommit{owner: s.control, id: submission, request: request}
	defer func() {
		cause := recover()
		var cleanupErr error
		if state != framehost.CommitSucceeded {
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

func (c installationCommit) conclude(ctx context.Context) (framehost.CommitState, error) {
	settlement, err := c.owner.settlePrepared(ctx, c.id, c.request)
	if err != nil || settlement.State != framehost.CommitRejected {
		err = errors.Join(ErrIndeterminate, ErrUnsettled, err)
		c.owner.store.admission.RequireRecovery(err)
		return framehost.CommitIndeterminate, err
	}
	if _, err := c.owner.synchronize(settlement, nil, confirmOnCommit); err != nil {
		return framehost.CommitRejected, err
	}
	return framehost.CommitRejected, c.owner.compactSettled(ctx)
}

func changedInstallation(change framehost.InstallationChange) (framehost.InstallationID, error) {
	var id framehost.InstallationID
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

func (s *storeAuthority) applyInstallation(ctx context.Context, tx Executor, id framehost.InstallationID, change framehost.InstallationChange) error {
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

func checkInstalled(ctx context.Context, tx Executor, id framehost.InstallationID, expected *framehost.InstallationRecord) error {
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

func cloneChange(change framehost.InstallationChange) framehost.InstallationChange {
	var projection framehost.InstallationChange
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

func (s *storeAuthority) ConfirmInstallation(ctx context.Context, id framehost.InstallationID) error {
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

func (s *storeAuthority) Installations(ctx context.Context) (records []framehost.InstallationRecord, err error) {
	err = s.transaction(ctx, func(tx Executor) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,record FROM frame_installations ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id framehost.InstallationID
			var data []byte
			if err := rows.Scan(&id, &data); err != nil {
				return err
			}
			var record framehost.InstallationRecord
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
func (s *storeAuthority) RecoverInstallations(ctx context.Context) ([]framehost.InstallationRecord, error) {
	if err := s.control.RecoverPrepared(ctx); err != nil {
		return nil, err
	}
	return s.Installations(ctx)
}

func (s *storeAuthority) ConfirmRestoredInstallations(ctx context.Context) error {
	return s.admission.Maintenance(ctx, func() error {
		if err := s.transaction(ctx, func(tx Executor) error {
			settlements, err := s.control.readSubmissions(ctx, tx)
			if err != nil {
				return err
			}
			for _, settlement := range settlements {
				if settlement.State != framehost.CommitIndeterminate {
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
