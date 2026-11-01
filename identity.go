package frame

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalid          = errors.New("frame: invalid value")
	ErrClosed           = errors.New("frame: lifetime closed")
	ErrStaleBinding     = errors.New("frame: stale binding")
	ErrUnavailable      = errors.New("frame: unavailable")
	ErrInUse            = errors.New("frame: installation in use")
	ErrAmbiguous        = errors.New("frame: ambiguous provider")
	ErrDependencyCycle  = errors.New("frame: dependency cycle")
	ErrContractMismatch = errors.New("frame: contract mismatch")
	ErrRecoveryRequired = errors.New("frame: recovery required")
	ErrFenced           = errors.New("frame: scope fenced")
	ErrCapacity         = errors.New("frame: capacity exceeded")
	ErrResyncRequired   = errors.New("frame: resynchronization required")
)

type InstallationID string
type UnitID string
type ScopeID string
type RealmID string
type Profile string

const MaxNameBytes = 256

const (
	NativeGo       Profile = "native-go"
	LocalProcess   Profile = "local-process"
	BrowserJS      Profile = "browser-js"
	StaticResource Profile = "static-resource"
)

func ValidName(value string) bool {
	return value != "" && len(value) <= MaxNameBytes && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func validateName(kind, value string) error {
	if !ValidName(value) {
		return fmt.Errorf("%w: %s", ErrInvalid, kind)
	}
	return nil
}

func (id InstallationID) Validate() error { return validateName("installation identity", string(id)) }
func (id UnitID) Validate() error         { return validateName("unit identity", string(id)) }
func (id ScopeID) Validate() error        { return validateName("scope identity", string(id)) }
func (id RealmID) Validate() error        { return validateName("realm identity", string(id)) }
func (p Profile) Validate() error         { return validateName("execution profile", string(p)) }
func (id InstallationID) String() string  { return string(id) }
func (id UnitID) String() string          { return string(id) }
func (id ScopeID) String() string         { return string(id) }
func (id RealmID) String() string         { return string(id) }

func ParseInstallationID(value string) (InstallationID, error) {
	id := InstallationID(value)
	return id, id.Validate()
}
