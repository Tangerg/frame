package runner

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

// ExitReceipt identifies the exact exit proof already committed by the Host.
type ExitReceipt struct {
	AttemptID string `json:"attemptId"`
	Epoch     uint64 `json:"epoch"`
	RunnerID  string `json:"runnerId"`
	ProofID   string `json:"proofId"`
}

func (receipt ExitReceipt) valid() bool {
	return validID(receipt.AttemptID) && receipt.Epoch != 0 && validID(receipt.RunnerID) && validID(receipt.ProofID)
}
func (receipt ExitReceipt) matches(o Observation) bool {
	return o.State == Exited && o.Epoch == receipt.Epoch && o.RunnerID == receipt.RunnerID && o.ProofID == receipt.ProofID
}
func (s *Server) receiptDirectory() string { return filepath.Join(s.config.Directory, "exit-receipts") }

func (s *Server) receiptPath(id string) string {
	return filepath.Join(s.receiptDirectory(), value.Hash([]byte(id)).String())
}

func (s *Server) lookupAttempt(id string) (storedAttempt, bool, error) {
	if item, ok := s.journal.Attempts[id]; ok {
		return item, true, nil
	}
	data, err := os.ReadFile(s.receiptPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return storedAttempt{}, false, nil
	}
	if err != nil {
		return storedAttempt{}, false, err
	}
	var item storedAttempt
	if err := json.Unmarshal(data, &item, json.RejectUnknownMembers(true)); err != nil {
		return storedAttempt{}, false, err
	}
	if item.Observation.AttemptID != id || item.Observation.State != Exited || item.Observation.ProofID == "" || item.RequestDigest.Validate() != nil {
		return storedAttempt{}, false, ErrConflict
	}
	return item, true, nil
}

// AcknowledgeExit moves a confirmed exit outside the outstanding-attempt budget.
// Its compact disk receipt retains only the original request digest and terminal
// observation. Attempt IDs remain single-use across epochs and runner restarts.
func (s *Server) AcknowledgeExit(ctx context.Context, receipt ExitReceipt) error {
	if !receipt.valid() {
		return frame.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrFenced
	}
	item, found, err := s.lookupAttempt(receipt.AttemptID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if !receipt.matches(item.Observation) {
		return ErrConflict
	}
	if _, outstanding := s.journal.Attempts[receipt.AttemptID]; !outstanding {
		return nil
	}
	if err := os.MkdirAll(s.receiptDirectory(), 0700); err != nil {
		return err
	}
	if err := storeRecord(s.receiptPath(receipt.AttemptID), item); err != nil {
		return err
	}
	// The receipt directory itself must be durable before the active journal
	// can omit this ID. Both copies mean the same immutable terminal observation.
	if err := syncDirectory(s.config.Directory); err != nil {
		return err
	}
	delete(s.journal.Attempts, receipt.AttemptID)
	if err := s.persist(); err != nil {
		s.journal.Attempts[receipt.AttemptID] = item
		s.fenced = true
		return err
	}
	return nil
}
