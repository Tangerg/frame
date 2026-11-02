package mcp

import (
	"errors"

	"github.com/Tangerg/frame/portable"
)

// Origin is consumer-defined evidence and grants no framework permission.
type Origin string

// Invocation is immutable calling-owner evidence. It contains no credentials,
// callback permissions, mutable execution state or business result.
type Invocation struct {
	ID     string          `json:"id"`
	Origin Origin          `json:"origin"`
	Parent string          `json:"parent,omitzero"`
	Target portable.Target `json:"target"`
}

func (i Invocation) Validate() error {
	if !portable.ValidToken(i.ID) || i.Parent != "" && (!portable.ValidToken(i.Parent) || i.Parent == i.ID) {
		return errors.New("mcp: invocation and distinct parent identities are required")
	}
	if !portable.ValidToken(string(i.Origin)) {
		return errors.New("mcp: invocation origin must be a bounded consumer token")
	}
	return i.Target.Validate()
}

var ErrInvocationConflict = errors.New("invocation identity conflicts with the original request")

// InvocationConflictCode is the refusal every method reports for
// ErrInvocationConflict, so no business owner restates it.
const InvocationConflictCode = "command_conflict"

// Refusal is a known business outcome, independent of transport failure.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string { return r.Message }

func (r *Refusal) Is(target error) bool {
	return target == ErrInvocationConflict && r.Code == InvocationConflictCode
}
