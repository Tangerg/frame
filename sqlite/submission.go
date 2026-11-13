package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type SubmissionID struct {
	Store    string `json:"store"`
	Owner    string `json:"owner"`
	Sequence uint64 `json:"sequence"`
}

type Settlement struct {
	ID           SubmissionID          `json:"id"`
	State        framehost.CommitState `json:"state"`
	Digest       value.Digest          `json:"digest"`
	Evidence     []byte                `json:"evidence,omitempty"`
	Synchronized bool                  `json:"synchronized"`
}

// Prepare issues an identity only after its sequence and original request are
// durable together. The identity alone grants no right to dispatch plugin work.
func (o Owner) Prepare(ctx context.Context, request []byte) (id SubmissionID, err error) {
	if err := o.validate(); err != nil {
		return id, err
	}
	err = o.store.admission.Admit(ctx, func(admitted context.Context) error {
		id, err = o.PrepareUnder(admitted, request)
		return err
	})
	return id, err
}

// PrepareUnder joins an existing bounded admission. The supplied context must
// belong to this Store's active authority; it does not reacquire the gate.
func (o Owner) PrepareUnder(ctx context.Context, request []byte) (SubmissionID, error) {
	if err := o.validate(); err != nil {
		return SubmissionID{}, err
	}
	if err := o.store.admission.CheckAdmitted(ctx); err != nil {
		return SubmissionID{}, err
	}
	return o.prepare(ctx, request)
}

func (o Owner) prepare(ctx context.Context, request []byte) (id SubmissionID, err error) {
	if err := o.validateRequest(request); err != nil {
		return id, err
	}
	request = bytes.Clone(request)
	err = func() error {
		var sequence uint64
		err := o.store.transaction(ctx, func(tx Executor) error {
			retained, err := o.retainedSubmissions(ctx, tx)
			if err != nil {
				return err
			}
			if retained >= uint64(o.store.maxSubmissions) {
				return frame.ErrCapacity
			}
			if err := tx.QueryRowContext(ctx, `UPDATE frame_owners SET next_sequence=next_sequence+1 WHERE id=? RETURNING next_sequence`, o.id).Scan(&sequence); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO frame_submissions(owner_id,sequence,digest,request,state) VALUES(?,?,?,?,?)`, o.id, sequence, value.Hash(request), request, submissionPrepared)
			return err
		})
		if err != nil {
			if errors.Is(err, ErrIndeterminate) {
				o.store.admission.RequireRecovery(err)
				return err
			}
			if sequence == 0 {
				return err
			}
			var stored []byte
			queryErr := o.store.db.QueryRowContext(context.Background(), `SELECT request FROM frame_submissions WHERE owner_id=? AND sequence=?`, o.id, sequence).Scan(&stored)
			if errors.Is(queryErr, sql.ErrNoRows) {
				return err
			}
			if queryErr != nil || !bytes.Equal(stored, request) {
				o.store.admission.RequireRecovery(errors.Join(err, queryErr))
				return errors.Join(ErrIndeterminate, err, queryErr)
			}
		}
		id = SubmissionID{Store: o.store.id, Owner: o.id, Sequence: sequence}
		return nil
	}()
	return id, err
}

// Commit never retries the mutation, including when the original response was
// lost. Evidence locates the accepted change; it is not a plugin result receipt.
// synchronize runs under the same admission after storage settles, before new
// work may observe a committed installation or business binding.
func (o Owner) Commit(ctx context.Context, id SubmissionID, request []byte, mutation func(context.Context, Executor) ([]byte, error), synchronize func(Settlement) error) (settlement Settlement, err error) {
	if err := o.validate(); err != nil {
		return settlement, err
	}
	err = o.store.admission.Admit(ctx, func(admitted context.Context) error {
		settlement, err = o.CommitUnder(admitted, id, request, mutation, synchronize)
		return err
	})
	return settlement, err
}

// CommitUnder shares the original admission with business validation and
// storage. It never admits a caller carrying an expired or foreign context.
func (o Owner) CommitUnder(ctx context.Context, id SubmissionID, request []byte, mutation func(context.Context, Executor) ([]byte, error), synchronize func(Settlement) error) (Settlement, error) {
	if err := o.validate(); err != nil {
		return Settlement{}, err
	}
	if err := o.store.admission.CheckAdmitted(ctx); err != nil {
		return Settlement{}, err
	}
	return o.commit(ctx, id, request, mutation, synchronize, confirmOnCommit)
}

// confirmation names who marks a committed submission synchronized: the
// commit itself after synchronize, or a caller that projects it separately.
type confirmation uint8

const (
	confirmOnCommit confirmation = iota
	confirmByCaller
)

type submissionMutation struct {
	owner      Owner
	id         SubmissionID
	request    []byte
	settlement Settlement
	entered    bool
}

func (o Owner) commit(ctx context.Context, id SubmissionID, request []byte, mutation func(context.Context, Executor) ([]byte, error), synchronize func(Settlement) error, confirm confirmation) (Settlement, error) {
	if err := o.validateSubmission(id); err != nil {
		return Settlement{}, err
	}
	if mutation == nil {
		return Settlement{}, errors.New("sqlite: mutation is required")
	}
	if err := o.validateRequest(request); err != nil {
		return Settlement{}, err
	}
	attempt := submissionMutation{owner: o, id: id, request: bytes.Clone(request)}
	if err := attempt.execute(ctx, mutation); err != nil {
		if err = attempt.resolveFailure(err); err != nil {
			return attempt.settlement, err
		}
	}
	return o.synchronize(attempt.settlement, synchronize, confirm)
}

func (m *submissionMutation) execute(ctx context.Context, mutate func(context.Context, Executor) ([]byte, error)) error {
	returned := false
	defer func() {
		cause := recover()
		if !returned {
			m.resolveInterruption()
		}
		if cause != nil {
			panic(cause)
		}
	}()
	err := m.owner.store.transaction(ctx, func(tx Executor) error {
		original, err := m.owner.readSettlement(ctx, tx, m.id, m.request)
		if err != nil {
			return err
		}
		m.settlement = original
		if original.State != framehost.CommitIndeterminate {
			return nil
		}
		m.entered = true
		var evidence []byte
		err = m.owner.withinSchema(tx, func(grant Executor) error {
			var err error
			evidence, err = mutate(ctx, grant)
			return err
		})
		if err != nil {
			return err
		}
		if len(evidence) > m.owner.store.maxRequestBytes {
			return errors.New("sqlite: settlement evidence exceeds its byte limit")
		}
		_, err = tx.ExecContext(ctx, `UPDATE frame_submissions SET state=?,evidence=? WHERE owner_id=? AND sequence=? AND digest=? AND state=?`, submissionCommitted, evidence, m.owner.id, m.id.Sequence, original.Digest, submissionPrepared)
		if err == nil {
			m.settlement.State = framehost.CommitSucceeded
			m.settlement.Evidence = bytes.Clone(evidence)
		}
		return err
	})
	returned = true
	return err
}

func (m *submissionMutation) resolveInterruption() {
	if !m.entered {
		return
	}
	original, err := m.owner.settlePrepared(context.Background(), m.id, m.request)
	m.settlement = original
	if err != nil {
		m.settlement = Settlement{ID: m.id, State: framehost.CommitIndeterminate}
	}
	if err == nil && original.State == framehost.CommitRejected {
		err = m.owner.confirm(context.Background(), m.id)
		if err == nil {
			m.settlement.Synchronized = true
		}
	}
	if err != nil || original.State == framehost.CommitSucceeded {
		m.owner.store.admission.RequireRecovery(errors.Join(ErrUnsettled, err))
	}
}

// Only a mutation whose original bytes matched can need reconciliation after
// a lost commit response. An identity read that failed grants no such authority.
func (m *submissionMutation) resolveFailure(failure error) error {
	if !m.entered {
		m.settlement = Settlement{}
		if errors.Is(failure, ErrIndeterminate) {
			m.owner.store.admission.RequireRecovery(failure)
		}
		return failure
	}
	if errors.Is(failure, ErrIndeterminate) {
		m.owner.store.admission.RequireRecovery(failure)
		m.settlement = Settlement{ID: m.id, Digest: m.settlement.Digest, State: framehost.CommitIndeterminate}
		return failure
	}
	original, err := m.owner.settlePrepared(context.Background(), m.id, m.request)
	if err != nil {
		m.settlement = Settlement{ID: m.id, State: framehost.CommitIndeterminate}
		m.owner.store.admission.RequireRecovery(err)
		return errors.Join(ErrIndeterminate, failure, err)
	}
	m.settlement = original
	if original.State == framehost.CommitSucceeded {
		return nil
	}
	m.settlement, err = m.owner.synchronize(original, nil, confirmOnCommit)
	return errors.Join(failure, err)
}

func (o Owner) synchronize(settlement Settlement, apply func(Settlement) error, confirm confirmation) (Settlement, error) {
	if settlement.Synchronized {
		return settlement, nil
	}
	returned := false
	defer func() {
		cause := recover()
		if !returned {
			o.store.admission.RequireRecovery(ErrUnsettled)
		}
		if cause != nil {
			panic(cause)
		}
	}()
	if apply != nil {
		projection := settlement
		projection.Evidence = bytes.Clone(settlement.Evidence)
		if err := apply(projection); err != nil {
			o.store.admission.RequireRecovery(err)
			returned = true
			return settlement, err
		}
	}
	if confirm == confirmOnCommit {
		if err := o.confirm(context.Background(), settlement.ID); err != nil {
			o.store.admission.RequireRecovery(err)
			returned = true
			return settlement, err
		}
		settlement.Synchronized = true
	}
	returned = true
	return settlement, nil
}

func (o Owner) readSettlement(ctx context.Context, tx Executor, id SubmissionID, request []byte) (Settlement, error) {
	result, stored, _, err := o.readSubmission(ctx, tx, id)
	if err != nil {
		return Settlement{}, err
	}
	if !bytes.Equal(request, stored) {
		return Settlement{}, ErrConflict
	}
	return result, nil
}

func (o Owner) Settlement(ctx context.Context, id SubmissionID, request []byte) (Settlement, error) {
	if err := o.validateSubmission(id); err != nil {
		return Settlement{}, err
	}
	var result Settlement
	err := o.store.transaction(ctx, func(tx Executor) error {
		var err error
		result, err = o.readSettlement(ctx, tx, id, request)
		return err
	})
	if err != nil {
		return Settlement{}, err
	}
	return result, nil
}

// RecoverPrepared is called after this Store acquired the exclusive authority.
// No transaction from the prior authority can still commit. It does not remove
// the Host barrier: business projections and execution scopes recover separately.
func (o Owner) RecoverPrepared(ctx context.Context) error {
	if err := o.validate(); err != nil {
		return err
	}
	return o.store.admission.Maintenance(ctx, func() error {
		return o.store.transaction(ctx, func(tx Executor) error {
			if _, err := o.readSubmissions(ctx, tx); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE frame_submissions SET state=? WHERE owner_id=? AND state=?`, submissionRejected, o.id, submissionPrepared)
			return err
		})
	})
}

func (o Owner) Pending(ctx context.Context) (settlements []Settlement, err error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	err = o.store.transaction(ctx, func(tx Executor) error {
		all, err := o.readSubmissions(ctx, tx)
		if err != nil {
			return err
		}
		for _, item := range all {
			if !item.Synchronized {
				settlements = append(settlements, item)
			}
		}
		return nil
	})
	return settlements, err
}

func (o Owner) Confirm(ctx context.Context, id SubmissionID) error {
	if err := o.validateSubmission(id); err != nil {
		return err
	}
	return o.store.admission.Maintenance(ctx, func() error { return o.confirm(ctx, id) })
}

func (o Owner) confirm(ctx context.Context, id SubmissionID) error {
	return o.store.transaction(ctx, func(tx Executor) error { return o.confirmIn(ctx, tx, id) })
}

func (o Owner) confirmIn(ctx context.Context, tx Executor, id SubmissionID) error {
	_, _, state, err := o.readSubmission(ctx, tx, id)
	if err != nil {
		return err
	}
	next, err := state.confirm()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE frame_submissions SET state=? WHERE owner_id=? AND sequence=?`, next, o.id, id.Sequence)
	return err
}

func (o Owner) RetainEvidence(ctx context.Context, id SubmissionID, holder string) error {
	if err := o.validateSubmission(id); err != nil {
		return err
	}
	if !frame.ValidName(holder) {
		return errors.New("sqlite: invalid evidence holder")
	}
	return o.store.admission.Under(ctx, func() error {
		return o.store.transaction(ctx, func(tx Executor) error {
			if _, _, _, err := o.readSubmission(ctx, tx, id); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO frame_evidence_references(owner_id,sequence,holder) VALUES(?,?,?)`, o.id, id.Sequence, holder)
			return err
		})
	})
}

func (o Owner) ReleaseEvidence(ctx context.Context, id SubmissionID, holder string) error {
	if err := o.validateSubmission(id); err != nil {
		return err
	}
	return o.store.admission.Maintenance(ctx, func() error {
		return o.store.transaction(ctx, func(tx Executor) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM frame_evidence_references WHERE owner_id=? AND sequence=? AND holder=?`, o.id, id.Sequence, holder)
			return err
		})
	})
}

func (o Owner) Compact(ctx context.Context, through uint64) error {
	if err := o.validate(); err != nil {
		return err
	}
	return o.store.admission.Maintenance(ctx, func() error { return o.compact(ctx, through) })
}

func (o Owner) compact(ctx context.Context, through uint64) error {
	return o.store.transaction(ctx, func(tx Executor) error {
		if _, err := o.readSubmissions(ctx, tx); err != nil {
			return err
		}
		var issued, compacted uint64
		if err := tx.QueryRowContext(ctx, `SELECT next_sequence,compacted_through FROM frame_owners WHERE id=?`, o.id).Scan(&issued, &compacted); err != nil {
			return err
		}
		if through < compacted || through > issued {
			return ErrUnknownSubmission
		}
		if through == compacted {
			return nil
		}
		var count uint64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM frame_submissions s WHERE owner_id=? AND sequence>? AND sequence<=? AND state IN (?,?) AND NOT EXISTS(SELECT 1 FROM frame_evidence_references r WHERE r.owner_id=s.owner_id AND r.sequence=s.sequence)`, o.id, compacted, through, submissionCommittedSynchronized, submissionRejectedSynchronized).Scan(&count); err != nil {
			return err
		}
		if count != through-compacted {
			return ErrUnsettled
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM frame_submissions WHERE owner_id=? AND sequence<=?`, o.id, through); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE frame_owners SET compacted_through=? WHERE id=?`, through, o.id)
		return err
	})
}

// CompactSettled reclaims only the contiguous prefix whose original owners
// confirmed synchronization and whose evidence is no longer retained.
func (o Owner) CompactSettled(ctx context.Context) error {
	if err := o.validate(); err != nil {
		return err
	}
	return o.store.admission.Maintenance(ctx, func() error { return o.compactSettled(ctx) })
}

func (o Owner) CompactSettledUnder(ctx context.Context) error {
	if err := o.validate(); err != nil {
		return err
	}
	if err := o.store.admission.CheckAdmitted(ctx); err != nil {
		return err
	}
	return o.compactSettled(ctx)
}

func (o Owner) compactSettled(ctx context.Context) error {
	var through uint64
	err := o.store.transaction(ctx, func(tx Executor) error {
		return tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT MIN(sequence)-1 FROM frame_submissions s WHERE owner_id=? AND (state NOT IN (?,?) OR EXISTS(SELECT 1 FROM frame_evidence_references r WHERE r.owner_id=s.owner_id AND r.sequence=s.sequence))),next_sequence) FROM frame_owners WHERE id=?`, o.id, submissionCommittedSynchronized, submissionRejectedSynchronized, o.id).Scan(&through)
	})
	if err != nil {
		return err
	}
	return o.compact(ctx, through)
}

func (o Owner) validate() error {
	if o.store == nil {
		return frame.ErrInvalid
	}
	return nil
}

func (o Owner) validateSubmission(id SubmissionID) error {
	if err := o.validate(); err != nil {
		return err
	}
	if id.Store != o.store.id || id.Owner != o.id || id.Sequence == 0 {
		return ErrUnknownSubmission
	}
	return nil
}

func (o Owner) validateRequest(request []byte) error {
	if len(request) == 0 {
		return fmt.Errorf("%w: sqlite submission request is empty", frame.ErrInvalid)
	}
	if len(request) > o.store.maxRequestBytes {
		return fmt.Errorf("%w: sqlite submission request exceeds its byte limit", frame.ErrCapacity)
	}
	return nil
}

func (o Owner) settlePrepared(ctx context.Context, id SubmissionID, request []byte) (Settlement, error) {
	var result Settlement
	err := o.store.transaction(ctx, func(tx Executor) error {
		var err error
		result, err = o.readSettlement(ctx, tx, id, request)
		if err != nil || result.State != framehost.CommitIndeterminate {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE frame_submissions SET state=? WHERE owner_id=? AND sequence=? AND digest=? AND state=?`, submissionRejected, o.id, id.Sequence, result.Digest, submissionPrepared)
		if err == nil {
			result.State = framehost.CommitRejected
		}
		return err
	})
	if err != nil {
		return Settlement{}, err
	}
	return result, nil
}

// Update admits a transition whose original business row already identifies
// the operation. The row's compare-and-set predicate determines if it can advance.
func (o Owner) Update(ctx context.Context, apply func(context.Context, Executor) error) error {
	if err := o.validate(); err != nil {
		return err
	}
	if apply == nil {
		return errors.New("sqlite: business update is required")
	}
	return o.store.admission.Admit(ctx, func(admitted context.Context) error {
		err := o.store.transaction(admitted, func(tx Executor) error {
			return o.withinSchema(tx, func(grant Executor) error { return apply(admitted, grant) })
		})
		if errors.Is(err, errCommitUnresolved) {
			err = errors.Join(ErrIndeterminate, err)
		}
		if errors.Is(err, ErrIndeterminate) {
			o.store.admission.RequireRecovery(err)
		}
		return err
	})
}
