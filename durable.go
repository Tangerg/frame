package frame

import (
	"reflect"

	"github.com/Tangerg/frame/internal/nilness"
)

// DurableAuthority ties execution evidence to the store that owns this Host's installation transitions.
type DurableAuthority interface {
	InstallationStore
	ExecutionProtection
	IsAuthority(*Admission, uint64) bool
}

func (h *Host) UsesAuthority(store DurableAuthority) bool {
	return h != nil && !nilness.Is(store) && reflect.TypeOf(store).Comparable() &&
		h.store == InstallationStore(store) && h.protection == ExecutionProtection(store) &&
		store.IsAuthority(h.admission, h.epoch)
}

func (s *Setup) UsesAuthority(store DurableAuthority) bool {
	return s != nil && s.host.UsesAuthority(store)
}
