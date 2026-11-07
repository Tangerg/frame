package runner

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/value"
)

// The Host persists this original exit identity before acknowledging it, because
// acknowledgement returns the runner's outstanding-attempt capacity.
type ExitReceipt struct {
	AttemptID string `json:"attemptId"`
	Epoch     uint64 `json:"epoch"`
	RunnerID  string `json:"runnerId"`
	ProofID   string `json:"proofId"`
	State     State  `json:"state"`
}

func (o Observation) ExitReceipt() (ExitReceipt, error) {
	if err := o.validate(); err != nil {
		return ExitReceipt{}, errors.Join(ErrUnknown, err)
	}
	receipt := ExitReceipt{AttemptID: o.AttemptID, Epoch: o.Epoch, RunnerID: o.RunnerID, ProofID: o.ProofID, State: o.State}
	if !receipt.valid() {
		return ExitReceipt{}, ErrUnknown
	}
	return receipt, nil
}

func (receipt ExitReceipt) valid() bool {
	return (receipt.State == Exited || receipt.State == NotCreated) && frame.ValidName(receipt.AttemptID) && receipt.Epoch != 0 && frame.ValidName(receipt.RunnerID) && frame.ValidName(receipt.ProofID)
}
func (receipt ExitReceipt) matches(o Observation) bool {
	return o.AttemptID == receipt.AttemptID && o.State == receipt.State && o.Epoch == receipt.Epoch && o.RunnerID == receipt.RunnerID && o.ProofID == receipt.ProofID
}
func (s *Server) receiptDirectory() string { return filepath.Join(s.config.Directory, "exit-receipts") }

func (s *Server) receiptPath(id string) string {
	return filepath.Join(s.receiptDirectory(), value.Hash([]byte(id)).String())
}

func (s *Server) lookupAttempt(id string) (storedAttempt, bool, error) {
	if item, ok := s.journal.Attempts[id]; ok {
		return item, true, nil
	}
	return s.readReceipt(id)
}

func (s *Server) readReceipt(id string) (storedAttempt, bool, error) {
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
	if _, err := item.Observation.ExitReceipt(); err != nil || item.Observation.AttemptID != id || item.RequestDigest.Validate() != nil {
		return storedAttempt{}, false, ErrConflict
	}
	return item, true, nil
}

func (s *Server) restoreReceipts() error {
	for id, outstanding := range s.journal.Attempts {
		receipt, found, err := s.readReceipt(id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if receipt.RequestDigest != outstanding.RequestDigest || receipt.Observation.Reference() != outstanding.Observation.Reference() {
			return ErrConflict
		}
		if outstanding.Observation.State == Exited {
			proof, err := receipt.Observation.ExitReceipt()
			if err != nil || !proof.matches(outstanding.Observation) {
				return ErrConflict
			}
		}
		// Receipt persistence precedes removal from the active journal. A crash
		// between those writes cannot erase the original terminal observation.
		delete(s.journal.Attempts, id)
	}
	return nil
}

// AcknowledgeExit moves a confirmed exit outside the outstanding-attempt budget.
// Its compact disk receipt retains only the original request digest and terminal
// observation. Attempt IDs remain single-use across epochs and runner restarts.
func (s *Server) AcknowledgeExit(ctx context.Context, receipt ExitReceipt) error {
	if !receipt.valid() {
		return frame.ErrInvalid
	}
	if err := cancellation.Err(ctx); err != nil {
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
	if err := s.storeReceipt(item); err != nil {
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

func (s *Server) storeReceipt(item storedAttempt) error {
	if err := os.MkdirAll(s.receiptDirectory(), localfile.PrivateDirectoryMode); err != nil {
		return err
	}
	if err := storeRecord(s.receiptPath(item.Observation.AttemptID), item); err != nil {
		return err
	}
	// The receipt directory itself must be durable before the active journal
	// can omit this ID. Both copies mean the same immutable terminal observation.
	return syncDirectory(s.config.Directory)
}
