package mcp

import "github.com/Tangerg/frame/call"

// InvocationConflictCode encodes call.ErrConflict in the managed refusal protocol.
const InvocationConflictCode = "command_conflict"

// A Refusal decoded from the original backend reply is a known business outcome.
// A locally constructed or wrapped Refusal cannot establish that remote fact.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string { return r.Message }

func (r *Refusal) Is(target error) bool {
	return target == call.ErrConflict && r.Code == InvocationConflictCode
}
