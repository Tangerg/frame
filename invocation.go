package frame

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Tangerg/frame/value"
)

const MaxCommandIDCharacters = 256

// The business caller owns command identity. Transports carry this immutable
// invocation without inventing a new identity after cancellation or relay.
type Invocation struct {
	member    string
	input     value.JSON
	commandID string
}

func NewQueryInvocation(member string, input value.JSON) (Invocation, error) {
	return newInvocation(member, input, "")
}

func NewCommandInvocation(commandID, member string, input value.JSON) (Invocation, error) {
	if !ValidCommandID(commandID) {
		return Invocation{}, errors.New("frame: bounded command identity required")
	}
	return newInvocation(member, input, commandID)
}

func newInvocation(member string, input value.JSON, commandID string) (Invocation, error) {
	if !ValidName(member) {
		return Invocation{}, errors.New("frame: invocation member must be a bounded token")
	}
	if err := input.Validate(); err != nil {
		return Invocation{}, fmt.Errorf("frame: invocation input: %w", err)
	}
	return Invocation{member: member, input: input, commandID: commandID}, nil
}

func ValidCommandID(id string) bool {
	return id != "" && utf8.ValidString(id) && utf8.RuneCountInString(id) <= MaxCommandIDCharacters
}

func (i Invocation) Member() string    { return i.member }
func (i Invocation) Input() value.JSON { return i.input }
func (i Invocation) CommandID() string { return i.commandID }
func (i Invocation) Kind() MethodKind {
	if i.commandID != "" {
		return Command
	}
	return Query
}

func (m *Method) ValidateInvocation(invocation Invocation) error {
	if m == nil || m.Name() != invocation.Member() || m.Kind() != invocation.Kind() {
		return errors.New("frame: invocation does not match the admitted method")
	}
	return m.ValidateInput(invocation.Input())
}
