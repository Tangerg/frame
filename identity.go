package frame

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalid          = errors.New("frame: invalid value")
	ErrClosed           = errors.New("frame: lifetime closed")
	ErrContractMismatch = errors.New("frame: contract mismatch")
	ErrCapacity         = errors.New("frame: capacity exceeded")
)

const MaxNameBytes = 256

func ValidName(value string) bool {
	return value != "" && len(value) <= MaxNameBytes && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
