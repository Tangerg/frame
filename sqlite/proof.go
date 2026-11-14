package sqlite

import (
	json "encoding/json/v2"
)

// MaxExitEvidenceBytes bounds the opaque evidence of one exit proof. Every
// other proof field is a bounded name, so any valid proof is storable: an
// execution admitted today can always record the proof that releases it,
// independently of the business request budget.
const MaxExitEvidenceBytes = 1 << 20

func validExitEvidence(evidence []byte) bool {
	return len(evidence) != 0 && len(evidence) <= MaxExitEvidenceBytes
}

// encodeProof spells a validated proof; validation already bounds its size.
func encodeProof(proof any) ([]byte, error) {
	return json.Marshal(proof)
}
