package runner

import (
	"context"
	"encoding/json"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

const absenceProofDomain = "runner-sealed-absence"

// AttemptRef binds creation and control to the authority approved before dispatch.
type AttemptRef struct {
	AttemptID string `json:"attemptId"`
	Epoch     uint64 `json:"epoch"`
	RunnerID  string `json:"runnerId"`
}

func (r AttemptRef) valid() bool {
	return frame.ValidName(r.AttemptID) && frame.ValidName(r.RunnerID) && r.Epoch != 0
}

type AbsenceProof struct {
	AttemptID  string `json:"attemptId"`
	Epoch      uint64 `json:"epoch"`
	RunnerID   string `json:"runnerId"`
	FenceEpoch uint64 `json:"fenceEpoch"`
	ProofID    string `json:"proofId"`
}

func (p AbsenceProof) Validate(request AttemptRef) error {
	if !request.valid() ||
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
func (s *Server) ProveAbsent(ctx context.Context, request AttemptRef) (AbsenceProof, error) {
	if err := frame.ContextError(ctx); err != nil {
		return AbsenceProof{}, err
	}
	if !request.valid() {
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

func absentProof(request AttemptRef, fenceEpoch uint64) (AbsenceProof, error) {
	encoded, err := json.Marshal(struct {
		Kind       string     `json:"kind"`
		Attempt    AttemptRef `json:"attempt"`
		FenceEpoch uint64     `json:"fenceEpoch"`
	}{absenceProofDomain, request, fenceEpoch})
	if err != nil {
		return AbsenceProof{}, err
	}
	return AbsenceProof{AttemptID: request.AttemptID, Epoch: request.Epoch, RunnerID: request.RunnerID, FenceEpoch: fenceEpoch, ProofID: value.Hash(encoded).String()}, nil
}
