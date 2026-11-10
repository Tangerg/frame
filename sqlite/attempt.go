package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"slices"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type Attempt struct {
	ID              string                `json:"id"`
	Epoch           uint64                `json:"epoch"`
	Source          frame.UnitRef         `json:"source"`
	Binding         frame.BindingSnapshot `json:"binding"`
	Realm           string                `json:"realm"`
	Profile         frame.Profile         `json:"profile"`
	Parent          string                `json:"parent,omitempty"`
	Scope           frame.ScopeID         `json:"scope,omitempty"`
	ScopeGeneration uint64                `json:"scopeGeneration,omitempty"`
	Generation      uint64                `json:"generation,omitempty"`
}

func (a Attempt) Validate() error {
	if !frame.ValidName(a.ID) || !frame.ValidName(a.Realm) || a.Epoch == 0 || a.Profile.Validate() != nil {
		return errors.New("sqlite: invalid execution attempt")
	}
	if a.Parent != "" && (a.Parent == a.ID || !frame.ValidName(a.Parent)) {
		return errors.New("sqlite: invalid parent attempt")
	}
	if a.Profile == frame.BrowserJS && (a.Scope.Validate() != nil || a.ScopeGeneration == 0 || a.Generation == 0) {
		return errors.New("sqlite: browser attempt requires its exact scope and generation")
	}
	if err := a.Source.Validate(); err != nil {
		return err
	}
	if err := a.Binding.Validate(); err != nil {
		return err
	}
	if !slices.ContainsFunc(a.Binding.Units, func(unit frame.UnitBinding) bool { return unit.Source == a.Source }) {
		return errors.New("sqlite: attempt source is outside its binding")
	}
	return nil
}

// ExitProof is accepted only from a trusted driver after it verifies its actual
// owner's evidence. Timeout, disconnect, a new epoch or an old PID is not proof.
type ExitProof struct {
	AttemptID string `json:"attemptId"`
	Realm     string `json:"realm"`
	ExitID    string `json:"exitId"`
	Kind      string `json:"kind"`
	Evidence  []byte `json:"evidence"`
}

func (p ExitProof) proves(attemptID string) bool {
	return frame.ValidName(attemptID) && p.AttemptID == attemptID && frame.ValidName(p.Realm) && frame.ValidName(p.ExitID) && frame.ValidName(p.Kind) &&
		len(p.Evidence) != 0
}

// LocalExit is the exit proof of an attempt that never started: its driver
// knows that outcome without external evidence.
func (a Attempt) LocalExit(kind, evidence string) ExitProof {
	return ExitProof{AttemptID: a.ID, Realm: a.Realm, ExitID: a.ID, Kind: kind, Evidence: []byte(evidence)}
}

type AttemptRecord struct {
	Attempt Attempt      `json:"attempt"`
	State   AttemptState `json:"state"`
	Proof   *ExitProof   `json:"proof,omitempty"`
}

// A runner acknowledgement may lag the original exit proof; both settled
// protection states must retain that proof before claiming exit.
func (r AttemptRecord) ExitProven() bool {
	return (r.State == AttemptHandoff || r.State == AttemptReleased) && r.Proof != nil &&
		r.Proof.proves(r.Attempt.ID) && r.Proof.Realm == r.Attempt.Realm
}

// RecordAttempt must complete before the runner or Realm can create anything.
// Reusing its identity never authorizes a second creation or a wider binding.
func (s *Store) RecordAttempt(ctx context.Context, attempt Attempt) error {
	if err := attempt.Validate(); err != nil {
		return err
	}
	if attempt.Epoch != s.epoch {
		return frame.ErrFenced
	}
	encoded, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	if len(encoded) > s.maxRequestBytes {
		return errors.New("sqlite: attempt exceeds its byte limit")
	}
	return s.admission.Under(ctx, func() error {
		return s.fenceUnlessRefused(s.transaction(ctx, func(tx Executor) error {
			return recordOnce(func() (bool, error) {
				row, err := readProtectionRow(ctx, tx, attemptByID, attempt.ID)
				if err != nil {
					return false, err
				}
				if _, err := decodeAttempt(row); err != nil {
					return false, err
				}
				return bytes.Equal(row.request, encoded), nil
			}, func() error { return insertAttempt(ctx, tx, attempt, encoded) })
		}))
	})
}

func insertAttempt(ctx context.Context, tx Executor, attempt Attempt, encoded []byte) error {
	if attempt.Parent != "" {
		row, err := readProtectionRow(ctx, tx, attemptByID, attempt.Parent)
		if err != nil {
			return err
		}
		parent, err := decodeAttempt(row)
		if err != nil {
			return err
		}
		if parent.State != AttemptApproved {
			return frame.ErrClosed
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO frame_attempts(id,request,request_digest,state) VALUES(?,?,?,?)`, attempt.ID, encoded, value.Hash(encoded), AttemptApproved)
	return err
}

const attemptRecords = `SELECT id,request,request_digest,state,proof FROM frame_attempts`
const attemptByID = attemptRecords + ` WHERE id=?`

func (s *Store) Attempts(ctx context.Context) ([]AttemptRecord, error) {
	return readRecords(ctx, s, attemptRecords+" ORDER BY id", decodeAttempt)
}
func (s *Store) PendingAttempts(ctx context.Context) ([]AttemptRecord, error) {
	records, err := s.Attempts(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(records, func(record AttemptRecord) bool { return !record.State.pending() }), nil
}
func (s *Store) Attempt(ctx context.Context, id string) (AttemptRecord, error) {
	return readRecord(ctx, s, attemptByID, decodeAttempt, id)
}

func decodeAttempt(row protectedRecord) (AttemptRecord, error) {
	var record AttemptRecord
	state := AttemptState(row.state)
	if err := state.validate(); err != nil {
		return record, err
	}
	if err := jsonv2.Unmarshal(row.request, &record.Attempt, jsonv2.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if err := record.Attempt.Validate(); err != nil {
		return record, err
	}
	if record.Attempt.ID != row.id {
		return record, errors.New("sqlite: execution attempt identity mismatch")
	}
	record.State = state
	if state == AttemptApproved {
		if len(row.proof) != 0 {
			return record, errors.New("sqlite: approved attempt has exit proof")
		}
		return record, nil
	}
	if err := jsonv2.Unmarshal(row.proof, &record.Proof, jsonv2.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if !record.ExitProven() {
		return record, errors.New("sqlite: invalid stored exit proof")
	}
	return record, nil
}

// ExecutionDependencies is deliberately separate from business Held queries.
// A Ready idle instance may be retired first; its protection remains here until
// actual exit is confirmed, including attempts with an unknown creation result.
func (s *Store) ExecutionDependencies(ctx context.Context) ([]frame.Dependency, error) {
	var dependencies []frame.Dependency
	err := s.Read(ctx, func(tx Reader) error {
		protection, err := readProtectionSnapshot(ctx, tx)
		if err != nil {
			return err
		}
		dependencies = protection.dependencies()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dependencies, nil
}

func (s *Store) ReleaseAttempt(ctx context.Context, id string, proof ExitProof) error {
	return s.recordRelease(ctx, id, proof, AttemptReleased)
}

// AcceptExit commits the original proof before its runner may reclaim capacity.
// Recovery retains the outstanding acknowledgement across either process crashing.
func (s *Store) AcceptExit(ctx context.Context, id string, proof ExitProof) error {
	return s.recordRelease(ctx, id, proof, AttemptHandoff)
}

func (s *Store) recordRelease(ctx context.Context, id string, proof ExitProof, target AttemptState) error {
	if !proof.proves(id) {
		return errors.New("sqlite: exact exit evidence is required")
	}
	encoded, err := s.encodeProof(proof)
	if err != nil {
		return err
	}
	return s.admission.Maintenance(ctx, func() error {
		return s.fenceUnlessRefused(s.transaction(ctx, func(tx Executor) error {
			row, err := readProtectionRow(ctx, tx, attemptByID, id)
			if err != nil {
				return err
			}
			record, err := decodeAttempt(row)
			if err != nil {
				return err
			}
			if record.Attempt.Realm != proof.Realm {
				return ErrConflict
			}
			next, err := record.State.afterProof(target)
			if err != nil {
				return err
			}
			if record.State != AttemptApproved && !bytes.Equal(row.proof, encoded) {
				return ErrConflict
			}
			if next == record.State {
				return nil
			}
			_, err = tx.ExecContext(ctx, `UPDATE frame_attempts SET state=?,proof=? WHERE id=? AND state=?`, next, encoded, id, record.State)
			return err
		}))
	})
}

// RecordExitProof records a verified subordinate exit without releasing the
// enclosing attempt. The driver owns scope validation; an acknowledgement can
// be repeated only for these exact original bytes.
func (s *Store) RecordExitProof(ctx context.Context, attemptID, scope, proofID string, evidence []byte) error {
	if !frame.ValidName(attemptID) || !frame.ValidName(scope) || !frame.ValidName(proofID) {
		return frame.ErrInvalid
	}
	if err := s.CheckExitEvidenceCapacity(evidence); err != nil {
		return err
	}
	evidence = bytes.Clone(evidence)
	return s.admission.Maintenance(ctx, func() error {
		return s.transaction(ctx, func(tx Executor) error {
			return recordOnce(func() (bool, error) {
				var prior []byte
				var priorID, priorScope string
				err := tx.QueryRowContext(ctx, `SELECT scope,proof_id,evidence FROM frame_exit_proofs WHERE attempt_id=? AND (proof_id=? OR scope=?)`, attemptID, proofID, scope).Scan(&priorScope, &priorID, &prior)
				return priorID == proofID && priorScope == scope && bytes.Equal(prior, evidence), err
			}, func() error {
				_, err := tx.ExecContext(ctx, `INSERT INTO frame_exit_proofs VALUES(?,?,?,?)`, attemptID, scope, proofID, evidence)
				return err
			})
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
