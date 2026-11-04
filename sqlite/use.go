package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"math"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

var _ frame.ExecutionProtection = (*Store)(nil)

func (s *Store) ApproveUse(ctx context.Context, approval frame.UseApproval) error {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return err
	}
	if !frame.ValidName(approval.ID) || approval.Realm.Validate() != nil || approval.Scope.Validate() != nil || approval.Epoch != s.epoch {
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
	if _, err := s.encodeProof(UseProof{Host: &frame.ReleaseProof{CloseID: math.MaxUint64}}); err != nil {
		return err
	}
	return s.fenceUnlessRefused(s.transaction(ctx, func(tx Executor) error {
		return recordOnce(func() (bool, error) {
			var previous []byte
			err := tx.QueryRowContext(ctx, `SELECT request FROM frame_use_approvals WHERE id=?`, approval.ID).Scan(&previous)
			return bytes.Equal(previous, encoded), err
		}, func() error { return insertUse(ctx, tx, approval, encoded) })
	}))
}

// insertUse approves a delegated Use only while its parent is approved.
func insertUse(ctx context.Context, tx Executor, approval frame.UseApproval, encoded []byte) error {
	if approval.ParentID != "" {
		var state useState
		if err := tx.QueryRowContext(ctx, `SELECT state FROM frame_use_approvals WHERE id=?`, approval.ParentID).Scan(&state); err != nil {
			return err
		}
		if err := state.validate(); err != nil {
			return err
		}
		if state != useApproved {
			return frame.ErrClosed
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO frame_use_approvals(id,request,request_digest,state) VALUES(?,?,?,?)`, approval.ID, encoded, value.Hash(encoded), useApproved)
	return err
}

type UseRecord struct {
	Approval frame.UseApproval
	Released bool
	Proof    *UseProof
}

type UseProof struct {
	Host  *frame.ReleaseProof `json:"host,omitempty"`
	Realm *ExitProof          `json:"realm,omitempty"`
}

const useRecords = `SELECT request,request_digest,state,proof FROM frame_use_approvals`
const protectedUses = useRecords + ` WHERE state=? ORDER BY id`

func (s *Store) Uses(ctx context.Context) ([]UseRecord, error) {
	return readRecords(ctx, s, useRecords+" ORDER BY id", decodeUse)
}
func (s *Store) Use(ctx context.Context, id string) (UseRecord, error) {
	return readRecord(ctx, s, useRecords+" WHERE id=?", decodeUse, id)
}
func (s *Store) ExecutionUses(ctx context.Context, id string) ([]UseRecord, error) {
	return readRecords(ctx, s, useRecords+` WHERE execution_id=? ORDER BY id`, decodeUse, id)
}

func decodeUse(row protectedRecord) (UseRecord, error) {
	var record UseRecord
	state := useState(row.state)
	if err := state.validate(); err != nil {
		return record, err
	}
	if err := jsonv2.Unmarshal(row.request, &record.Approval, jsonv2.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if err := record.Approval.Binding.Validate(); err != nil {
		return record, err
	}
	record.Released = state == useReleased
	if len(row.proof) != 0 {
		if err := jsonv2.Unmarshal(row.proof, &record.Proof, jsonv2.RejectUnknownMembers(true)); err != nil {
			return record, err
		}
	}
	if record.Released && (record.Proof == nil || (record.Proof.Host == nil) == (record.Proof.Realm == nil) || record.Proof.Host != nil && record.Proof.Host.CloseID == 0) {
		return record, errors.New("sqlite: released use lacks proof")
	}
	return record, nil
}

func (s *Store) ReleaseUse(ctx context.Context, id string, proof frame.ReleaseProof) error {
	if !frame.ValidName(id) || proof.CloseID == 0 {
		return frame.ErrInvalid
	}
	encoded, err := s.encodeProof(UseProof{Host: &proof})
	if err != nil {
		return err
	}
	return s.admission.Maintenance(ctx, func() error {
		return s.fenceUnlessRefused(s.transaction(ctx, func(tx Executor) error {
			var state useState
			var previous []byte
			if err := tx.QueryRowContext(ctx, `SELECT state,proof FROM frame_use_approvals WHERE id=?`, id).Scan(&state, &previous); err != nil {
				return err
			}
			if err := state.validate(); err != nil {
				return err
			}
			if state == useReleased {
				if !bytes.Equal(previous, encoded) {
					return ErrConflict
				}
				return nil
			}
			_, err := tx.ExecContext(ctx, `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, useReleased, encoded, id)
			return err
		}))
	})
}

// ReleaseRealmUse settles an original delegated use after Host reconstruction.
// The trusted bridge verifies the actual Realm proof; it cannot release another
// execution which merely shares the Realm or installation scope.
func (s *Store) ReleaseRealmUse(ctx context.Context, id string, realm frame.RealmID, executionID string, proof ExitProof) error {
	if !frame.ValidName(id) || realm.Validate() != nil || !proof.proves(executionID) || proof.Realm != string(realm) {
		return frame.ErrInvalid
	}
	encoded, err := s.encodeProof(UseProof{Realm: &proof})
	if err != nil {
		return err
	}
	return s.admission.Maintenance(ctx, func() error {
		return s.fenceUnlessRefused(s.transaction(ctx, func(tx Executor) error {
			var request, prior []byte
			var state useState
			if err := tx.QueryRowContext(ctx, `SELECT request,state,proof FROM frame_use_approvals WHERE id=?`, id).Scan(&request, &state, &prior); err != nil {
				return err
			}
			var approval frame.UseApproval
			if err := json.Unmarshal(request, &approval); err != nil {
				return err
			}
			if approval.Realm != realm || approval.ExecutionID != executionID || approval.Epoch >= s.epoch {
				return ErrConflict
			}
			if err := state.validate(); err != nil {
				return err
			}
			if state == useReleased {
				if !bytes.Equal(prior, encoded) {
					return ErrConflict
				}
				return nil
			}
			_, err := tx.ExecContext(ctx, `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, useReleased, encoded, id)
			return err
		}))
	})
}
