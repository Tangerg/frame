package sqlite

import (
	"context"
	"errors"

	framehost "github.com/Tangerg/frame/host"
)

// Protection has no submission journal to reconcile a lost COMMIT. The
// transaction owner must fence that unknown outcome before returning admission.
// A confirmed rollback leaves authority intact unless stored evidence is corrupt.
func (s *storeAuthority) protectionTransaction(ctx context.Context, apply func(Executor) error) (framehost.CommitState, error) {
	state, err := s.transact(ctx, writeTransaction, apply, s.admission.RequireRecovery)
	if errors.Is(err, errProtectionIntegrity) {
		s.admission.RequireRecovery(err)
	}
	return state, err
}
