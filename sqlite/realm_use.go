package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/Tangerg/frame"
)

// ReleaseRealmUse settles an original delegated use after Host reconstruction.
// The trusted bridge verifies the actual Realm proof; it cannot release another
// execution which merely shares the Realm or installation scope.
func (s *Store) ReleaseRealmUse(ctx context.Context, id string, realm frame.RealmID, executionID string, proof ExitProof) error {
	if !validID(id) || realm.Validate() != nil || !validID(executionID) || !validID(proof.ExitID) || len(proof.Evidence) == 0 {
		return frame.ErrInvalid
	}
	encoded, err := json.Marshal(UseProof{Realm: &proof})
	if err != nil {
		return err
	}
	if len(encoded) > s.maxRequestBytes {
		return frame.ErrCapacity
	}
	return s.admission.Maintenance(ctx, func() error {
		return s.transaction(ctx, func(tx Executor) error {
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
			if proof.AttemptID != executionID || proof.Realm != string(realm) {
				return errors.New("sqlite: realm proof does not identify the original execution")
			}
			_, err := tx.ExecContext(ctx, `UPDATE frame_use_approvals SET state=?,proof=? WHERE id=?`, useReleased, encoded, id)
			return err
		})
	})
}
