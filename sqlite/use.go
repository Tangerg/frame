package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	json "encoding/json/v2"
	"errors"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

var _ framehost.ExecutionProtection = Store{}

func (s *storeAuthority) ApproveUse(ctx context.Context, approval framehost.UseApproval) (framehost.CommitState, error) {
	if err := s.admission.CheckAdmitted(ctx); err != nil {
		return framehost.NotCommitted, err
	}
	if err := approval.Validate(); err != nil {
		return framehost.NotCommitted, err
	}
	if approval.Epoch != s.epoch {
		return framehost.NotCommitted, framehost.ErrFenced
	}
	encoded, err := json.Marshal(approval)
	if err != nil {
		return framehost.NotCommitted, err
	}
	return recordProtection(ctx, s, useByID, approval.ID, encoded, protectedRecord.use, func(tx Executor) error {
		return insertUse(ctx, tx, approval, encoded)
	})
}

func insertUse(ctx context.Context, tx Executor, approval framehost.UseApproval, encoded []byte) error {
	if approval.ParentID != "" {
		row, err := readProtectionRow(ctx, tx, useByID, approval.ParentID)
		if err != nil {
			return err
		}
		parent, err := row.use()
		if err != nil {
			return err
		}
		if parent.Released() {
			return frame.ErrClosed
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO frame_use_approvals(id,request,request_digest,state) VALUES(?,?,?,?)`, approval.ID, encoded, value.Hash(encoded), useApproved)
	return err
}

// UseRecord is one stored approval. Its release proof is present exactly when
// the approval has been released.
type UseRecord struct {
	Approval framehost.UseApproval
	Proof    *UseProof
}

func (r UseRecord) Released() bool { return r.Proof != nil }

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
	return readRecords(ctx, s, useRecords+" ORDER BY id", protectedRecord.use)
}
func (s *storeAuthority) Use(ctx context.Context, id string) (UseRecord, error) {
	return readRecord(ctx, s, useByID, protectedRecord.use, id)
}
func (s *storeAuthority) ExecutionUses(ctx context.Context, id string) ([]UseRecord, error) {
	return readRecords(ctx, s, useRecords+` WHERE execution_id=? ORDER BY id`, protectedRecord.use, id)
}

// use decodes this row as a Use approval; any inconsistency is corruption.
func (r protectedRecord) use() (record UseRecord, err error) {
	defer func() { err = corruptProtection(err) }()
	state := useState(r.state)
	if err := state.validate(); err != nil {
		return record, err
	}
	if err := json.Unmarshal(r.request, &record.Approval, json.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if err := record.Approval.Validate(); err != nil {
		return record, err
	}
	if record.Approval.ID != r.id {
		return record, errors.New("sqlite: execution use identity mismatch")
	}
	if state == useApproved {
		if len(r.proof) != 0 {
			return record, errors.New("sqlite: approved use has release proof")
		}
		return record, nil
	}
	var proof UseProof
	if err := json.Unmarshal(r.proof, &proof, json.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if !proof.proves(record.Approval) {
		return record, errors.New("sqlite: released use lacks proof")
	}
	record.Proof = &proof
	return record, nil
}

func (s *storeAuthority) SettleUse(ctx context.Context, id string, proof frame.ReleaseProof) error {
	if !frame.ValidName(id) || proof.CloseID == 0 {
		return frame.ErrInvalid
	}
	encoded, err := json.Marshal(UseProof{Host: &proof})
	if err != nil {
		return err
	}
	return s.admission.Maintenance(ctx, func() error {
		_, err := s.protectionTransaction(ctx, func(tx Executor) error {
			row, err := readProtectionRow(ctx, tx, useByID, id)
			// The original approval has finished, and this write transaction
			// excludes every earlier writer. Absence is therefore conclusive.
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			record, err := row.use()
			if err != nil {
				return err
			}
			if record.Released() {
				if !bytes.Equal(row.proof, encoded) {
					return ErrConflict
				}
				return nil
			}
			_, err = tx.ExecContext(ctx, `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, useReleased, encoded, id)
			return err
		})
		return err
	})
}

// ReleaseRealmUse settles an original delegated use after Host reconstruction.
// The trusted bridge verifies the actual Realm proof; it cannot release another
// execution which merely shares the Realm or installation scope.
func (s *storeAuthority) ReleaseRealmUse(ctx context.Context, id string, realm framehost.RealmID, executionID string, proof ExitProof) error {
	if !frame.ValidName(id) || realm.Validate() != nil || !proof.proves(executionID) || proof.Realm != string(realm) {
		return frame.ErrInvalid
	}
	encoded, err := json.Marshal(UseProof{Realm: &proof})
	if err != nil {
		return err
	}
	return s.admission.Maintenance(ctx, func() error {
		_, err := s.protectionTransaction(ctx, func(tx Executor) error {
			row, err := readProtectionRow(ctx, tx, useByID, id)
			if err != nil {
				return err
			}
			record, err := row.use()
			if err != nil {
				return err
			}
			approval := record.Approval
			if approval.Realm != realm || approval.ExecutionID != executionID || approval.Epoch >= s.epoch {
				return ErrConflict
			}
			if record.Released() {
				if !bytes.Equal(row.proof, encoded) {
					return ErrConflict
				}
				return nil
			}
			_, err = tx.ExecContext(ctx, `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, useReleased, encoded, id)
			return err
		})
		return err
	})
}
