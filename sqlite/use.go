package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"math"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

var _ framehost.ExecutionProtection = Store{}

func (s *storeAuthority) ApproveUse(ctx context.Context, approval framehost.UseApproval) error {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return err
	}
	if err := approval.Validate(); err != nil {
		return err
	}
	if approval.Epoch != s.epoch {
		return framehost.ErrFenced
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
			row, err := readProtectionRow(ctx, tx, useByID, approval.ID)
			if err != nil {
				return false, err
			}
			if _, err := decodeUse(row); err != nil {
				return false, err
			}
			return bytes.Equal(row.request, encoded), nil
		}, func() error { return insertUse(ctx, tx, approval, encoded) })
	}))
}

func insertUse(ctx context.Context, tx Executor, approval framehost.UseApproval, encoded []byte) error {
	if approval.ParentID != "" {
		row, err := readProtectionRow(ctx, tx, useByID, approval.ParentID)
		if err != nil {
			return err
		}
		parent, err := decodeUse(row)
		if err != nil {
			return err
		}
		if parent.Released {
			return frame.ErrClosed
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO frame_use_approvals(id,request,request_digest,state) VALUES(?,?,?,?)`, approval.ID, encoded, value.Hash(encoded), useApproved)
	return err
}

type UseRecord struct {
	Approval framehost.UseApproval
	Released bool
	Proof    *UseProof
}

type UseProof struct {
	Host  *frame.ReleaseProof `json:"host,omitempty"`
	Realm *ExitProof          `json:"realm,omitempty"`
}

func (p *UseProof) proves(approval framehost.UseApproval) bool {
	if p == nil || (p.Host == nil) == (p.Realm == nil) {
		return false
	}
	if p.Host != nil {
		return p.Host.CloseID != 0
	}
	return p.Realm.proves(approval.ExecutionID) && p.Realm.Realm == string(approval.Realm)
}

const useRecords = `SELECT id,request,request_digest,state,proof FROM frame_use_approvals`
const useByID = useRecords + ` WHERE id=?`

func (s *storeAuthority) Uses(ctx context.Context) ([]UseRecord, error) {
	return readRecords(ctx, s, useRecords+" ORDER BY id", decodeUse)
}
func (s *storeAuthority) Use(ctx context.Context, id string) (UseRecord, error) {
	return readRecord(ctx, s, useByID, decodeUse, id)
}
func (s *storeAuthority) ExecutionUses(ctx context.Context, id string) ([]UseRecord, error) {
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
	if err := record.Approval.Validate(); err != nil {
		return record, err
	}
	if record.Approval.ID != row.id {
		return record, errors.New("sqlite: execution use identity mismatch")
	}
	record.Released = state == useReleased
	if !record.Released {
		if len(row.proof) != 0 {
			return record, errors.New("sqlite: approved use has release proof")
		}
		return record, nil
	}
	if err := jsonv2.Unmarshal(row.proof, &record.Proof, jsonv2.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if !record.Proof.proves(record.Approval) {
		return record, errors.New("sqlite: released use lacks proof")
	}
	return record, nil
}

func (s *storeAuthority) SettleUse(ctx context.Context, id string, proof frame.ReleaseProof) error {
	if !frame.ValidName(id) || proof.CloseID == 0 {
		return frame.ErrInvalid
	}
	encoded, err := s.encodeProof(UseProof{Host: &proof})
	if err != nil {
		return err
	}
	return s.admission.Maintenance(ctx, func() error {
		return s.fenceUnlessRefused(s.transaction(ctx, func(tx Executor) error {
			row, err := readProtectionRow(ctx, tx, useByID, id)
			// The original approval has finished, and this write transaction
			// excludes every earlier writer. Absence is therefore conclusive.
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			record, err := decodeUse(row)
			if err != nil {
				return err
			}
			if record.Released {
				if !bytes.Equal(row.proof, encoded) {
					return ErrConflict
				}
				return nil
			}
			_, err = tx.ExecContext(ctx, `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, useReleased, encoded, id)
			return err
		}))
	})
}

// ReleaseRealmUse settles an original delegated use after Host reconstruction.
// The trusted bridge verifies the actual Realm proof; it cannot release another
// execution which merely shares the Realm or installation scope.
func (s *storeAuthority) ReleaseRealmUse(ctx context.Context, id string, realm framehost.RealmID, executionID string, proof ExitProof) error {
	if !frame.ValidName(id) || realm.Validate() != nil || !proof.proves(executionID) || proof.Realm != string(realm) {
		return frame.ErrInvalid
	}
	encoded, err := s.encodeProof(UseProof{Realm: &proof})
	if err != nil {
		return err
	}
	return s.admission.Maintenance(ctx, func() error {
		return s.fenceUnlessRefused(s.transaction(ctx, func(tx Executor) error {
			row, err := readProtectionRow(ctx, tx, useByID, id)
			if err != nil {
				return err
			}
			record, err := decodeUse(row)
			if err != nil {
				return err
			}
			approval := record.Approval
			if approval.Realm != realm || approval.ExecutionID != executionID || approval.Epoch >= s.epoch {
				return ErrConflict
			}
			if record.Released {
				if !bytes.Equal(row.proof, encoded) {
					return ErrConflict
				}
				return nil
			}
			_, err = tx.ExecContext(ctx, `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, useReleased, encoded, id)
			return err
		}))
	})
}
