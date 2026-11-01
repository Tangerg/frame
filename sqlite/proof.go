package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/Tangerg/frame"
)

// RecordExitProof records a verified subordinate exit without releasing the
// enclosing attempt. The driver owns scope validation; an acknowledgement can
// be repeated only for these exact original bytes.
func (s *Store) RecordExitProof(ctx context.Context, attemptID, scope, proofID string, evidence []byte) error {
	if !validID(attemptID) || !validID(scope) || !validID(proofID) || len(evidence) == 0 || len(evidence) > s.maxRequestBytes {
		return frame.ErrInvalid
	}
	evidence = bytes.Clone(evidence)
	return s.admission.Maintenance(ctx, func() error {
		return s.transaction(ctx, func(tx Executor) error {
			var prior []byte
			var priorID, priorScope string
			err := tx.QueryRowContext(ctx, `SELECT scope,proof_id,evidence FROM frame_exit_proofs WHERE attempt_id=? AND (proof_id=? OR scope=?)`, attemptID, proofID, scope).Scan(&priorScope, &priorID, &prior)
			if err == nil {
				if priorID != proofID || priorScope != scope || !bytes.Equal(prior, evidence) {
					return ErrConflict
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO frame_exit_proofs VALUES(?,?,?,?)`, attemptID, scope, proofID, evidence)
			return err
		})
	})
}

func (s *Store) ExitProof(ctx context.Context, attemptID, proofID string) ([]byte, error) {
	var evidence []byte
	err := s.transaction(ctx, func(tx Executor) error {
		return tx.QueryRowContext(ctx, `SELECT evidence FROM frame_exit_proofs WHERE attempt_id=? AND proof_id=?`, attemptID, proofID).Scan(&evidence)
	})
	return evidence, err
}
