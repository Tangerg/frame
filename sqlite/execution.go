package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
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
	if !validID(a.ID) || !validID(a.Realm) || a.Epoch == 0 || a.Profile.Validate() != nil {
		return errors.New("sqlite: invalid execution attempt")
	}
	if a.Parent != "" && !validID(a.Parent) {
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

type AttemptRecord struct {
	Attempt        Attempt    `json:"attempt"`
	Released       bool       `json:"released"`
	PendingHandoff bool       `json:"pendingHandoff"`
	Proof          *ExitProof `json:"proof,omitempty"`
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
		err := s.transaction(ctx, func(tx Executor) error {
			var previous []byte
			err := tx.QueryRowContext(ctx, `SELECT request FROM frame_attempts WHERE id=?`, attempt.ID).Scan(&previous)
			if err == nil {
				if !bytes.Equal(previous, encoded) {
					return ErrConflict
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if attempt.Parent != "" {
				var state string
				if err := tx.QueryRowContext(ctx, `SELECT state FROM frame_attempts WHERE id=?`, attempt.Parent).Scan(&state); err != nil {
					return err
				}
				if state != "approved" {
					return frame.ErrClosed
				}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO frame_attempts(id,epoch,request,request_digest,state) VALUES(?,?,?,?,'approved')`, attempt.ID, attempt.Epoch, encoded, value.Hash(encoded))
			return err
		})
		if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, frame.ErrClosed) {
			s.admission.RequireRecovery(err)
		}
		return err
	})
}

const attemptRecords = `SELECT request,request_digest,state,proof FROM frame_attempts`
const protectedAttempts = attemptRecords + ` WHERE state='approved' ORDER BY id`

func (s *Store) Attempts(ctx context.Context) ([]AttemptRecord, error) {
	return readRecords(ctx, s, attemptRecords+" ORDER BY id", decodeAttempt)
}
func (s *Store) PendingAttempts(ctx context.Context) ([]AttemptRecord, error) {
	return readRecords(ctx, s, attemptRecords+" WHERE state IN ('approved','handoff') ORDER BY id", decodeAttempt)
}
func (s *Store) Attempt(ctx context.Context, id string) (AttemptRecord, error) {
	return readRecord(ctx, s, attemptRecords+" WHERE id=?", decodeAttempt, id)
}

type protectedRecord struct {
	request, proof []byte
	state          string
}

func decodeAttempt(row protectedRecord) (AttemptRecord, error) {
	var record AttemptRecord
	if err := jsonv2.Unmarshal(row.request, &record.Attempt, jsonv2.RejectUnknownMembers(true)); err != nil {
		return record, err
	}
	if err := record.Attempt.Validate(); err != nil {
		return record, err
	}
	record.PendingHandoff = row.state == "handoff"
	record.Released = record.PendingHandoff || row.state == "released"
	if record.Released {
		if err := jsonv2.Unmarshal(row.proof, &record.Proof, jsonv2.RejectUnknownMembers(true)); err != nil {
			return record, err
		}
		if record.Proof == nil || record.Proof.AttemptID != record.Attempt.ID || record.Proof.Realm != record.Attempt.Realm {
			return record, errors.New("sqlite: invalid stored exit proof")
		}
	}
	return record, nil
}

func readRecord[T any](ctx context.Context, store *Store, query string, decode func(protectedRecord) (T, error), id string) (T, error) {
	var zero T
	records, err := readRecords(ctx, store, query, decode, id)
	if err != nil {
		return zero, err
	}
	if len(records) == 0 {
		return zero, sql.ErrNoRows
	}
	return records[0], nil
}

func readRecords[T any](ctx context.Context, store *Store, query string, decode func(protectedRecord) (T, error), args ...any) ([]T, error) {
	var records []T
	err := store.Read(ctx, func(tx Executor) error {
		var err error
		records, err = scanRecords(ctx, tx, query, decode, args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func scanRecords[T any](ctx context.Context, tx Executor, query string, decode func(protectedRecord) (T, error), args ...any) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []T
	for rows.Next() {
		var row protectedRecord
		var digest value.Digest
		if err := rows.Scan(&row.request, &digest, &row.state, &row.proof); err != nil {
			return nil, err
		}
		if value.Hash(row.request) != digest {
			return nil, errors.New("sqlite: execution protection integrity failure")
		}
		record, err := decode(row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// ExecutionDependencies is deliberately separate from business Held queries.
// A Ready idle instance may be retired first; its protection remains here until
// actual exit is confirmed, including attempts with an unknown creation result.
func (s *Store) ExecutionDependencies(ctx context.Context) ([]frame.Dependency, error) {
	var dependencies []frame.Dependency
	err := s.Read(ctx, func(tx Executor) error {
		attempts, err := scanRecords(ctx, tx, protectedAttempts, decodeAttempt)
		if err != nil {
			return err
		}
		for _, record := range attempts {
			dependencies = append(dependencies, record.Attempt.Binding.Dependencies()...)
		}
		uses, err := scanRecords(ctx, tx, protectedUses, decodeUse)
		if err != nil {
			return err
		}
		for _, record := range uses {
			dependencies = append(dependencies, record.Approval.Binding.Dependencies()...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return frame.CompactDependencies(dependencies), nil
}

func (s *Store) ReleaseAttempt(ctx context.Context, id string, proof ExitProof) error {
	return s.recordRelease(ctx, id, proof, "released")
}

// AcceptExit commits the original proof before its runner may reclaim capacity.
// Recovery retains the outstanding acknowledgement across either process crashing.
func (s *Store) AcceptExit(ctx context.Context, id string, proof ExitProof) error {
	return s.recordRelease(ctx, id, proof, "handoff")
}

func (s *Store) recordRelease(ctx context.Context, id string, proof ExitProof, target string) error {
	if !validID(id) || proof.AttemptID != id || !validID(proof.Realm) || !validID(proof.ExitID) || !validID(proof.Kind) || len(proof.Evidence) == 0 || len(proof.Evidence) > s.maxRequestBytes {
		return errors.New("sqlite: exact exit evidence is required")
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	return s.admission.Maintenance(ctx, func() error {
		err := s.transaction(ctx, func(tx Executor) error {
			var request, previous []byte
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT request,state,proof FROM frame_attempts WHERE id=?`, id).Scan(&request, &state, &previous); err != nil {
				return err
			}
			var attempt Attempt
			if err := jsonv2.Unmarshal(request, &attempt); err != nil {
				return err
			}
			if attempt.Realm != proof.Realm {
				return ErrConflict
			}
			if state == "released" || state == "handoff" {
				if !bytes.Equal(previous, encoded) {
					return ErrConflict
				}
				if state == "handoff" && target == "released" {
					_, err := tx.ExecContext(ctx, `UPDATE frame_attempts SET state='released' WHERE id=?`, id)
					return err
				}
				return nil
			}
			_, err := tx.ExecContext(ctx, `UPDATE frame_attempts SET state=?,proof=? WHERE id=? AND state='approved'`, target, encoded, id)
			return err
		})
		if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, sql.ErrNoRows) {
			s.admission.RequireRecovery(fmt.Errorf("persist exit proof: %w", err))
		}
		return err
	})
}

// Read invokes a trusted query in a fresh current transaction. It is useful for
// recovery and diagnostics; mutations must use an Owner's submission protocol.
func (s *Store) Read(ctx context.Context, query func(Executor) error) error {
	if query == nil {
		return errors.New("sqlite: query is required")
	}
	return s.transact(ctx, readTransaction, query)
}
