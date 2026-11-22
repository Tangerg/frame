package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	json "encoding/json/v2"
	"errors"
	"slices"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

// Attempt is one durable execution request. It pins the exact preparation,
// including the scope generation and instance generation it runs under, so a
// fenced scope or a successor instance cannot reuse its protection.
type Attempt struct {
	ID              string                    `json:"id"`
	Epoch           uint64                    `json:"epoch"`
	Source          framehost.UnitRef         `json:"source"`
	Binding         framehost.BindingSnapshot `json:"binding"`
	Realm           string                    `json:"realm"`
	Profile         framehost.Profile         `json:"profile"`
	Scope           framehost.ScopeID         `json:"scope"`
	ScopeGeneration uint64                    `json:"scopeGeneration"`
	Generation      uint64                    `json:"generation"`
}

// NewAttempt projects the original preparation onto the request that protects
// its execution; realm names the authority that will own that execution.
func NewAttempt(setup framehost.Setup, realm string) Attempt {
	grant := setup.ScopeGrant()
	return Attempt{
		ID: setup.InstanceID(), Epoch: setup.Epoch(), Source: setup.Binding().Source, Binding: setup.Snapshot(),
		Realm: realm, Profile: setup.Unit().Profile,
		Scope: grant.WireID(), ScopeGeneration: grant.Generation(), Generation: setup.Generation(),
	}
}

func (a Attempt) Validate() error {
	if !frame.ValidName(a.ID) || !frame.ValidName(a.Realm) || a.Epoch == 0 || a.Profile.Validate() != nil {
		return errors.New("sqlite: invalid execution attempt")
	}
	if a.Scope.Validate() != nil || a.ScopeGeneration == 0 || a.Generation == 0 {
		return errors.New("sqlite: attempt requires its exact scope and generations")
	}
	if err := a.Source.Validate(); err != nil {
		return err
	}
	if err := a.Binding.Validate(); err != nil {
		return err
	}
	if !slices.ContainsFunc(a.Binding.Units, func(unit framehost.UnitBinding) bool { return unit.Source == a.Source }) {
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
		validExitEvidence(p.Evidence)
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
// A failed acknowledgement retains the original cleanup obligation unless the
// returned conclusion is NotCommitted; callers must not retry creation.
func (s *storeAuthority) RecordAttempt(ctx context.Context, attempt Attempt) (framehost.CommitState, error) {
	if err := attempt.Validate(); err != nil {
		return framehost.NotCommitted, err
	}
	if attempt.Epoch != s.epoch {
		return framehost.NotCommitted, framehost.ErrFenced
	}
	encoded, err := json.Marshal(attempt)
	if err != nil {
		return framehost.NotCommitted, err
	}
	state := framehost.NotCommitted
	err = s.admission.Under(ctx, func() error {
		var err error
		state, err = recordProtection(ctx, s, attemptByID, attempt.ID, encoded, protectedRecord.attempt, func(tx Executor) error {
			return insertAttempt(ctx, tx, attempt, encoded)
		})
		return err
	})
	return state, err
}

func insertAttempt(ctx context.Context, tx Executor, attempt Attempt, encoded []byte) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO frame_attempts(id,request,request_digest,state) VALUES(?,?,?,?)`, attempt.ID, encoded, value.Hash(encoded), AttemptApproved)
	return err
}

const attemptRecords = `SELECT id,request,request_digest,state,proof FROM frame_attempts`
const attemptByID = attemptRecords + ` WHERE id=?`

func (s *storeAuthority) Attempts(ctx context.Context) ([]AttemptRecord, error) {
	return readRecords(ctx, s, attemptRecords+" ORDER BY id", protectedRecord.attempt)
}
func (s *storeAuthority) PendingAttempts(ctx context.Context) ([]AttemptRecord, error) {
	records, err := s.Attempts(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(records, func(record AttemptRecord) bool { return !record.State.pending() }), nil
}
func (s *storeAuthority) Attempt(ctx context.Context, id string) (AttemptRecord, error) {
	return readRecord(ctx, s, attemptByID, protectedRecord.attempt, id)
}

// attempt decodes this row as an attempt; any inconsistency is corruption.
func (r protectedRecord) attempt() (record AttemptRecord, err error) {
	defer func() { err = corruptProtection(err) }()
	state := AttemptState(r.state)
	if err := state.validate(); err != nil {
		return record, err
	}
	if err := json.Unmarshal(r.request, &record.Attempt, json.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if err := record.Attempt.Validate(); err != nil {
		return record, err
	}
	if record.Attempt.ID != r.id {
		return record, errors.New("sqlite: execution attempt identity mismatch")
	}
	record.State = state
	if state == AttemptApproved {
		if len(r.proof) != 0 {
			return record, errors.New("sqlite: approved attempt has exit proof")
		}
		return record, nil
	}
	if err := json.Unmarshal(r.proof, &record.Proof, json.RejectUnknownMembers(true)); err != nil {
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
func (s *storeAuthority) ExecutionDependencies(ctx context.Context) ([]framehost.ReleaseRef, error) {
	var dependencies []framehost.ReleaseRef
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

func (s *storeAuthority) ReleaseAttempt(ctx context.Context, id string, proof ExitProof) error {
	return s.recordRelease(ctx, id, proof, AttemptReleased, nil)
}

// SettleAttempt joins a completed approval whose acknowledgement may be lost.
// The caller must own the original exit or non-dispatch evidence. Its writer
// barrier proves absence, and the exact request prevents settling another attempt.
func (s *storeAuthority) SettleAttempt(ctx context.Context, attempt Attempt, proof ExitProof) error {
	if err := attempt.Validate(); err != nil {
		return err
	}
	if proof.Realm != attempt.Realm {
		return ErrConflict
	}
	original, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	return s.recordRelease(ctx, attempt.ID, proof, AttemptReleased, original)
}

// AcceptExit commits the original proof before its runner may reclaim capacity.
// Recovery retains the outstanding acknowledgement across either process crashing.
func (s *storeAuthority) AcceptExit(ctx context.Context, id string, proof ExitProof) error {
	return s.recordRelease(ctx, id, proof, AttemptHandoff, nil)
}

func (s *storeAuthority) recordRelease(ctx context.Context, id string, proof ExitProof, target AttemptState, original []byte) error {
	if !proof.proves(id) {
		return errors.New("sqlite: exact exit evidence is required")
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	release := attemptRelease{proof: proof, encoded: encoded, target: target, original: original}
	return s.admission.Maintenance(ctx, func() error {
		_, err := s.protectionTransaction(ctx, func(tx Executor) error { return release.apply(ctx, tx) })
		return err
	})
}

// attemptRelease advances one attempt by its exact proof. A nil original
// addresses the attempt by ID alone; otherwise only those request bytes may.
type attemptRelease struct {
	proof    ExitProof
	encoded  []byte
	target   AttemptState
	original []byte
}

func (r attemptRelease) apply(ctx context.Context, tx Executor) error {
	row, err := readProtectionRow(ctx, tx, attemptByID, r.proof.AttemptID)
	if errors.Is(err, sql.ErrNoRows) && r.original != nil {
		return nil
	}
	if err != nil {
		return err
	}
	if r.original != nil && !bytes.Equal(row.request, r.original) {
		return ErrConflict
	}
	record, err := row.attempt()
	if err != nil {
		return err
	}
	if record.Attempt.Realm != r.proof.Realm {
		return ErrConflict
	}
	next, err := record.State.afterProof(r.target)
	if err != nil {
		return err
	}
	if record.State != AttemptApproved && !bytes.Equal(row.proof, r.encoded) {
		return ErrConflict
	}
	if next == record.State {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE frame_attempts SET state=?,proof=? WHERE id=? AND state=?`, next, r.encoded, r.proof.AttemptID, record.State)
	return err
}

// RecordExitProof records a verified subordinate exit without releasing the
// enclosing attempt. The driver owns scope validation; an acknowledgement can
// be repeated only for these exact original bytes.
func (s *storeAuthority) RecordExitProof(ctx context.Context, attemptID, scope, proofID string, evidence []byte) error {
	if !frame.ValidName(attemptID) || !frame.ValidName(scope) || !frame.ValidName(proofID) {
		return frame.ErrInvalid
	}
	if !validExitEvidence(evidence) {
		return frame.ErrInvalid
	}
	evidence = bytes.Clone(evidence)
	return s.admission.Maintenance(ctx, func() error {
		_, err := s.protectionTransaction(ctx, func(tx Executor) error {
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
		return err
	})
}

func (s *storeAuthority) ExitProof(ctx context.Context, attemptID, proofID string) ([]byte, error) {
	var evidence []byte
	err := s.transaction(ctx, func(tx Executor) error {
		return tx.QueryRowContext(ctx, `SELECT evidence FROM frame_exit_proofs WHERE attempt_id=? AND proof_id=?`, attemptID, proofID).Scan(&evidence)
	})
	return evidence, err
}
