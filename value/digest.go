// Package value defines immutable wire values and their content identities.
package value

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var ErrInvalidDigest = errors.New("value: invalid sha256 digest")

// Digest is one SHA-256 value in its canonical lowercase hexadecimal spelling.
// Hosts, releases, contracts and their consumers share this one representation; the
// private text makes a Digest constructible only through [ParseDigest] or
// hashing. The zero value is "no digest".
type Digest struct{ text string }

const digestHexBytes = 2 * sha256.Size

var digestExpression = regexp.MustCompile(fmt.Sprintf(`^[0-9a-f]{%d}$`, digestHexBytes))

// DigestPattern projects the canonical spelling for public contracts.
func DigestPattern() string { return digestExpression.String() }

func ParseDigest(value string) (Digest, error) {
	if !digestExpression.MatchString(value) {
		return Digest{}, ErrInvalidDigest
	}
	return Digest{text: value}, nil
}

func Hash(value []byte) Digest { return DigestOf(sha256.Sum256(value)) }

func DigestOf(sum [sha256.Size]byte) Digest { return Digest{text: hex.EncodeToString(sum[:])} }

func (d Digest) String() string { return d.text }
func (d Digest) IsZero() bool   { return d.text == "" }

func (d Digest) Compare(other Digest) int { return strings.Compare(d.text, other.text) }

func (d Digest) Validate() error {
	if d.IsZero() {
		return ErrInvalidDigest
	}
	return nil
}

// Empty text encodes the zero value, so a stored "no digest" decodes to exactly
// what was encoded and every other spelling is parsed.
func (d Digest) MarshalText() ([]byte, error) { return []byte(d.text), nil }

func (d *Digest) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*d = Digest{}
		return nil
	}
	parsed, err := ParseDigest(string(text))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Stores bind and scan the same text encoding, so persistence cannot hold a
// spelling that parsing would reject.
func (d Digest) Value() (driver.Value, error) { return d.text, nil }

func (d *Digest) Scan(source any) error {
	switch value := source.(type) {
	case string:
		return d.UnmarshalText([]byte(value))
	case []byte:
		return d.UnmarshalText(value)
	default:
		return fmt.Errorf("%w: column %T", ErrInvalidDigest, source)
	}
}

// digestModel is the pattern-constrained string a Digest marshals to; schema
// derivation describes that text wire shape instead of the private struct.
type digestModel string

func (digestModel) JSONSchemaPattern() string { return DigestPattern() }

func (Digest) JSONSchemaModel() any { return digestModel("") }
