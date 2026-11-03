package sqlite

import (
	"context"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

// RealmCredential stores only an authentication verifier. It is not release,
// configuration or diagnostic content and is never projected to plugin code.
type RealmCredential struct {
	ID             string
	Epoch          string
	OperationID    string
	ResumeVerifier value.Digest
}

func (s *Store) RegisterRealm(ctx context.Context, credential RealmCredential) error {
	if !frame.ValidName(credential.ID) || !frame.ValidName(credential.Epoch) || !frame.ValidName(credential.OperationID) || credential.ResumeVerifier.Validate() != nil {
		return frame.ErrInvalid
	}
	return s.admission.Under(ctx, func() error {
		return s.transaction(ctx, func(tx Executor) error {
			return recordOnce(func() (bool, error) {
				var previous RealmCredential
				err := tx.QueryRowContext(ctx, `SELECT id,epoch,operation_id,resume_verifier FROM frame_realms WHERE (id=? AND epoch=?) OR operation_id=?`, credential.ID, credential.Epoch, credential.OperationID).Scan(&previous.ID, &previous.Epoch, &previous.OperationID, &previous.ResumeVerifier)
				return previous == credential, err
			}, func() error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO frame_realms(id,epoch,operation_id,resume_verifier) VALUES(?,?,?,?)`, credential.ID, credential.Epoch, credential.OperationID, credential.ResumeVerifier); err != nil {
					return err
				}
				return s.checkRealmHistoryCapacity(ctx, tx)
			})
		})
	})
}

func (s *Store) RealmCredential(ctx context.Context, id, epoch string) (RealmCredential, error) {
	if !frame.ValidName(id) || !frame.ValidName(epoch) {
		return RealmCredential{}, frame.ErrInvalid
	}
	credential := RealmCredential{ID: id, Epoch: epoch}
	err := s.transaction(ctx, func(tx Executor) error {
		return tx.QueryRowContext(ctx, `SELECT operation_id,resume_verifier FROM frame_realms WHERE id=? AND epoch=?`, id, epoch).Scan(&credential.OperationID, &credential.ResumeVerifier)
	})
	return credential, err
}

func (s *Store) RealmOperation(ctx context.Context, operationID string) (RealmCredential, error) {
	if !frame.ValidName(operationID) {
		return RealmCredential{}, frame.ErrInvalid
	}
	credential := RealmCredential{OperationID: operationID}
	err := s.transaction(ctx, func(tx Executor) error {
		return tx.QueryRowContext(ctx, `SELECT id,epoch,resume_verifier FROM frame_realms WHERE operation_id=?`, operationID).Scan(&credential.ID, &credential.Epoch, &credential.ResumeVerifier)
	})
	return credential, err
}
