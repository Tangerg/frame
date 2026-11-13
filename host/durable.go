package host

import (
	"fmt"
	"reflect"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// DurableAuthority ties execution evidence to the store that owns this Host's installation transitions.
type DurableAuthority interface {
	InstallationStore
	ExecutionProtection
	IsAuthority(Admission, uint64) bool
}

func (h *hostState) validateDurableAuthority() error {
	installation, installationDurable := h.store.(DurableAuthority)
	protection, protectionDurable := h.protection.(DurableAuthority)
	if installationDurable && !installation.IsAuthority(h.admission, h.epoch) {
		return fmt.Errorf("%w: installation authority differs from host admission or epoch", frame.ErrInvalid)
	}
	if protectionDurable && !protection.IsAuthority(h.admission, h.epoch) {
		return fmt.Errorf("%w: execution protection authority differs from host admission or epoch", frame.ErrInvalid)
	}
	if installationDurable && protectionDurable && !h.UsesAuthority(installation) {
		return fmt.Errorf("%w: installation and execution protection require the same durable authority", frame.ErrInvalid)
	}
	return nil
}

func (h *hostState) UsesAuthority(store DurableAuthority) bool {
	return h != nil && !framevalue.IsNil(store) && reflect.TypeOf(store).Comparable() &&
		h.store == InstallationStore(store) && h.protection == ExecutionProtection(store) &&
		store.IsAuthority(h.admission, h.epoch)
}

func (s *Setup) UsesAuthority(store DurableAuthority) bool {
	return s != nil && s.setupCall != nil && s.host.UsesAuthority(store)
}
