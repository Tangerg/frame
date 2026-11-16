package call

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Tangerg/frame"
)

// Target is a concrete reference interpreted by the operation's resource owner.
// Scope names the operation's target vocabulary, not a UI selection.
type Target struct {
	Scope     string `json:"scope"`
	Reference string `json:"reference"`
}

func (t Target) Validate() error {
	if !frame.ValidName(t.Scope) || t.Reference == "" || !utf8.ValidString(t.Reference) || strings.ContainsFunc(t.Reference, unicode.IsControl) {
		return errors.New("call: concrete target scope and reference are required")
	}
	return nil
}

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
	if !frame.ValidName(i.ID) || i.Parent != "" && (!frame.ValidName(i.Parent) || i.Parent == i.ID) {
		return errors.New("call: invocation and distinct parent identities are required")
	}
	if !frame.ValidName(string(i.Origin)) {
		return errors.New("call: invocation origin must be a bounded consumer token")
	}
	return i.Target.Validate()
}

var ErrConflict = errors.New("call: invocation identity conflicts with the original request")
