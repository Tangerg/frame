package runner

import (
	"context"
	"encoding/json"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
)

// AbsenceRequest names the original authority and creation epoch of an attempt
// recorded before dispatch. A missing entry alone is not non-creation evidence.
type AbsenceRequest struct {
	AttemptID string `json:"attemptId"`
	Epoch     uint64 `json:"epoch"`
	RunnerID  string `json:"runnerId"`
}

type AbsenceProof struct {
	AttemptID  string `json:"attemptId"`
	Epoch      uint64 `json:"epoch"`
	RunnerID   string `json:"runnerId"`
	FenceEpoch uint64 `json:"fenceEpoch"`
	ProofID    string `json:"proofId"`
}

func (p AbsenceProof) Validate(request AbsenceRequest) error {
	if !validID(request.AttemptID) || !validID(request.RunnerID) || request.Epoch == 0 ||
		p.AttemptID != request.AttemptID || p.Epoch != request.Epoch || p.RunnerID != request.RunnerID || p.FenceEpoch <= p.Epoch {
		return ErrUnknown
	}
	expected, err := absentProof(request, p.FenceEpoch)
	if err != nil || p.ProofID != expected.ProofID {
		return ErrUnknown
	}
	return nil
}

// ProveAbsent reads the original journal and its durable creation fence under
// one lock. A replacement authority cannot prove absence for its predecessor,
// including when it replaces that authority between a client's control calls.
func (s *Server) ProveAbsent(ctx context.Context, request AbsenceRequest) (AbsenceProof, error) {
	if err := ctx.Err(); err != nil {
		return AbsenceProof{}, err
	}
	if !validID(request.AttemptID) || !validID(request.RunnerID) || request.Epoch == 0 {
		return AbsenceProof{}, frame.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.fenced || s.id != request.RunnerID || s.journal.Epoch <= request.Epoch {
		return AbsenceProof{}, ErrUnknown
	}
	if _, exists, err := s.lookupAttempt(request.AttemptID); err != nil || exists {
		return AbsenceProof{}, ErrUnknown
	}
	return absentProof(request, s.journal.Epoch)
}

func absentProof(request AbsenceRequest, fenceEpoch uint64) (AbsenceProof, error) {
	encoded, err := json.Marshal(struct {
		Kind       string         `json:"kind"`
		Attempt    AbsenceRequest `json:"attempt"`
		FenceEpoch uint64         `json:"fenceEpoch"`
	}{"runner-sealed-absence", request, fenceEpoch})
	if err != nil {
		return AbsenceProof{}, err
	}
	return AbsenceProof{AttemptID: request.AttemptID, Epoch: request.Epoch, RunnerID: request.RunnerID, FenceEpoch: fenceEpoch, ProofID: integrity.Hash(encoded).String()}, nil
}
