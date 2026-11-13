package host

import (
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
)

type InstallationID string
type UnitID string
type ScopeID string
type RealmID string
type Profile string

const (
	NativeGo       Profile = "native-go"
	LocalProcess   Profile = "local-process"
	BrowserJS      Profile = "browser-js"
	StaticResource Profile = "static-resource"
)

func validateName(kind, value string) error {
	if !frame.ValidName(value) {
		return fmt.Errorf("%w: %s", frame.ErrInvalid, kind)
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

var (
	ErrStaleBinding     = errors.New("frame: stale binding")
	ErrUnavailable      = errors.New("frame: unavailable")
	ErrInUse            = errors.New("frame: installation in use")
	ErrAmbiguous        = errors.New("frame: ambiguous provider")
	ErrDependencyCycle  = errors.New("frame: dependency cycle")
	ErrRecoveryRequired = errors.New("frame: recovery required")
	ErrFenced           = errors.New("frame: scope fenced")
	ErrResyncRequired   = errors.New("frame: resynchronization required")
)
