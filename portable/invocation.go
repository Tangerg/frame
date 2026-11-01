package portable

import "errors"

// Origin is consumer-defined evidence and grants no framework permission.
type Origin string

// Invocation is immutable calling-owner evidence. It contains no credentials,
// callback permissions, mutable execution state or business result.
type Invocation struct {
	ID     string `json:"id"`
	Origin Origin `json:"origin"`
	Parent string `json:"parent,omitzero"`
	Target Target `json:"target"`
}

func (i Invocation) Validate() error {
	if !validToken(i.ID) || i.Parent != "" && (!validToken(i.Parent) || i.Parent == i.ID) {
		return errors.New("capability: invocation and distinct parent identities are required")
	}
	if !validToken(string(i.Origin)) {
		return errors.New("capability: invocation origin must be a bounded consumer token")
	}
	return i.Target.Validate()
}
