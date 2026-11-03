package browser

import "encoding/json"

type HTTPEnvelope struct {
	Method string          `json:"method"`
	Input  json.RawMessage `json:"input"`
}

type HTTPProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
