package control

import "encoding/json"

// HTTPEnvelope carries one control operation. Input is decoded as that
// operation's published DTO by the authenticated host adapter.
type HTTPEnvelope struct {
	Method string          `json:"method"`
	Input  json.RawMessage `json:"input"`
}

type HTTPProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
