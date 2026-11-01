package process

import (
	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/distribution"
)

// ValidateArchiveUnit admits the same executable archive shape for each stdio protocol.
// Protocol negotiation runs later, against the approved package and live peer.
func (d *Driver) ValidateArchiveUnit(unit distribution.Unit) error {
	if unit.Profile != d.Profile() {
		return frame.ErrContractMismatch
	}
	return unit.ValidateProcessEntry()
}
