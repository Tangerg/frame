package browser

import "encoding/json/jsontext"

type HTTPEnvelope struct {
	Method string         `json:"method"`
	Input  jsontext.Value `json:"input"`
}

type HTTPProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
