package portable

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Target is a concrete reference interpreted by the operation's resource owner.
// Scope is the method contract's target vocabulary, not a UI selection.
type Target struct {
	Scope     string `json:"scope"`
	Reference string `json:"reference"`
}

func (t Target) Validate() error {
	if !validToken(t.Scope) || t.Reference == "" || !utf8.ValidString(t.Reference) || strings.ContainsFunc(t.Reference, unicode.IsControl) {
		return errors.New("capability: concrete target scope and reference are required")
	}
	return nil
}
