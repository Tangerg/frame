package sqlite

import (
	"encoding/json"

	"github.com/Tangerg/frame"
)

func (s *Store) encodeProof(proof any) ([]byte, error) {
	encoded, err := json.Marshal(proof)
	if err != nil {
		return nil, err
	}
	if len(encoded) > s.maxRequestBytes {
		return nil, frame.ErrCapacity
	}
	return encoded, nil
}

func (s *Store) CheckExitEvidenceCapacity(evidence []byte) error {
	if len(evidence) == 0 {
		return frame.ErrInvalid
	}
	if len(evidence) > s.maxRequestBytes {
		return frame.ErrCapacity
	}
	return nil
}

func (s *Store) CheckExitCapacity(proof ExitProof) error {
	if !proof.proves(proof.AttemptID) {
		return frame.ErrInvalid
	}
	_, err := s.encodeProof(proof)
	return err
}

// A Realm may outlive its Host, so admission must include the envelope used
// when that original proof releases reconstructed Uses.
func (s *Store) CheckRealmUseExitCapacity(proof ExitProof) error {
	if !proof.proves(proof.AttemptID) {
		return frame.ErrInvalid
	}
	_, err := s.encodeProof(UseProof{Realm: &proof})
	return err
}
