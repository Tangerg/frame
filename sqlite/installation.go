package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

var _ frame.InstallationStore = (*Store)(nil)
var _ frame.ExecutionProtection = (*Store)(nil)

func (s *Store) CommitInstallation(ctx context.Context, change frame.InstallationChange) (frame.CommitState, error) {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return frame.CommitRejected, err
	}
	var id frame.InstallationID
	if change.Before != nil {
		id = change.Before.ID
	}
	if change.After != nil {
		if id != "" && id != change.After.ID {
			return frame.CommitRejected, ErrConflict
		}
		id = change.After.ID
	}
	if err := id.Validate(); err != nil {
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
	settlement, err := s.control.commit(ctx, submission, request, func(ctx context.Context, tx Executor) ([]byte, error) {
		var stored []byte
		readErr := tx.QueryRowContext(ctx, `SELECT record FROM frame_installations WHERE id=?`, id).Scan(&stored)
		if change.Before == nil {
			if readErr == nil {
				return nil, ErrConflict
			}
			if !errors.Is(readErr, sql.ErrNoRows) {
				return nil, readErr
			}
		} else {
			if readErr != nil {
				return nil, readErr
			}
			expected, err := json.Marshal(change.Before)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(stored, expected) {
				return nil, ErrConflict
			}
		}
		if change.After == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM frame_installations WHERE id=?`, id); err != nil {
				return nil, err
			}
		} else {
			encoded, err := json.Marshal(change.After)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO frame_installations(id,record) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET record=excluded.record`, id, encoded); err != nil {
				return nil, err
			}
		}
		if s.projectInstallation != nil {
			projection := frame.InstallationChange{}
			if change.Before != nil {
				before := change.Before.Clone()
				projection.Before = &before
			}
			if change.After != nil {
				after := change.After.Clone()
				projection.After = &after
			}
			if err := s.projectInstallation(ctx, tx, projection); err != nil {
				return nil, err
			}
		}
		return []byte(id), nil
	}, nil, false)
	switch settlement.State {
	case Committed:
		return frame.CommitSucceeded, err
	case NotCommitted:
		return frame.CommitRejected, err
	default:
		return frame.CommitIndeterminate, err
	}
}

func (s *Store) ConfirmInstallation(ctx context.Context, id frame.InstallationID) error {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return err
	}
	if err := s.transaction(ctx, func(tx Executor) error {
		rows, err := tx.QueryContext(ctx, `SELECT sequence FROM frame_submissions WHERE owner_id=? AND state='committed' AND synchronized=0 AND evidence=? ORDER BY sequence`, s.control.id, []byte(id))
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
			_, err := tx.ExecContext(ctx, `UPDATE frame_submissions SET synchronized=1 WHERE owner_id=? AND state!='prepared'`, s.control.id)
			return err
		}); err != nil {
			return err
		}
		return s.control.compactSettled(ctx)
	})
}

func (s *Store) ApproveUse(ctx context.Context, approval frame.UseApproval) error {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return err
	}
	if !validID(approval.ID) || approval.Realm.Validate() != nil || approval.Scope.Validate() != nil || approval.Epoch != s.epoch {
		return frame.ErrInvalid
	}
	if err := approval.Binding.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(approval)
	if err != nil {
		return err
	}
	if len(encoded) > s.maxRequestBytes {
		return frame.ErrCapacity
	}
	err = s.transaction(ctx, func(tx Executor) error {
		var previous []byte
		err := tx.QueryRowContext(ctx, `SELECT request FROM frame_use_approvals WHERE id=?`, approval.ID).Scan(&previous)
		if err == nil {
			if !bytes.Equal(previous, encoded) {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if approval.ParentID != "" {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM frame_use_approvals WHERE id=?`, approval.ParentID).Scan(&state); err != nil {
				return err
			}
			if state != "approved" {
				return frame.ErrClosed
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO frame_use_approvals(id,request,request_digest,state) VALUES(?,?,?,'approved')`, approval.ID, encoded, value.Hash(encoded))
		return err
	})
	if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, frame.ErrClosed) {
		s.admission.RequireRecovery(err)
	}
	return err
}

type UseRecord struct {
	Approval frame.UseApproval
	Released bool
	Proof    *UseProof
}

type UseProof struct {
	Host  *frame.CloseOutcome `json:"host,omitempty"`
	Realm *ExitProof          `json:"realm,omitempty"`
}

func (s *Store) Uses(ctx context.Context) (records []UseRecord, err error) {
	err = s.transaction(ctx, func(tx Executor) error {
		rows, err := tx.QueryContext(ctx, `SELECT request,request_digest,state,proof FROM frame_use_approvals ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var request, proof []byte
			var digest value.Digest
			var state string
			if err := rows.Scan(&request, &digest, &state, &proof); err != nil {
				return err
			}
			if value.Hash(request) != digest {
				return errors.New("sqlite: use approval integrity failure")
			}
			var record UseRecord
			if err := jsonv2.Unmarshal(request, &record.Approval, jsonv2.RejectUnknownMembers(true)); err != nil {
				return err
			}
			if err := record.Approval.Binding.Validate(); err != nil {
				return err
			}
			record.Released = state == "released"
			if len(proof) != 0 {
				if err := jsonv2.Unmarshal(proof, &record.Proof, jsonv2.RejectUnknownMembers(true)); err != nil {
					return err
				}
			}
			if record.Released && (record.Proof == nil || record.Proof.Host == nil && record.Proof.Realm == nil || record.Proof.Host != nil && !record.Proof.Host.Released) {
				return errors.New("sqlite: released use lacks proof")
			}
			records = append(records, record)
		}
		return rows.Err()
	})
	return records, err
}

func (s *Store) ReleaseUse(ctx context.Context, id string, outcome frame.CloseOutcome) error {
	if !validID(id) || outcome.ID == 0 {
		return frame.ErrInvalid
	}
	encoded, err := json.Marshal(UseProof{Host: &outcome})
	if err != nil {
		return err
	}
	if len(encoded) > s.maxRequestBytes {
		return frame.ErrCapacity
	}
	return s.admission.Maintenance(ctx, func() error {
		err := s.transaction(ctx, func(tx Executor) error {
			var state string
			var previous []byte
			if err := tx.QueryRowContext(ctx, `SELECT state,proof FROM frame_use_approvals WHERE id=?`, id).Scan(&state, &previous); err != nil {
				return err
			}
			if state == "released" {
				if !bytes.Equal(previous, encoded) {
					return ErrConflict
				}
				return nil
			}
			if len(previous) != 0 {
				var prior UseProof
				if err := json.Unmarshal(previous, &prior); err != nil {
					return err
				}
				if prior.Host == nil || prior.Host.ID != outcome.ID {
					return ErrConflict
				}
			}
			if outcome.Released {
				state = "released"
			}
			_, err := tx.ExecContext(ctx, `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, state, encoded, id)
			return err
		})
		if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, sql.ErrNoRows) {
			s.admission.RequireRecovery(err)
		}
		return err
	})
}
