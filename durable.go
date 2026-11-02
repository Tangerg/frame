package frame

import (
	"errors"
	"reflect"
)

// DurableAuthority ties execution evidence to the store that owns this Host's installation transitions.
type DurableAuthority interface {
	InstallationStore
	ExecutionProtection
	IsAuthority(*Admission, uint64) bool
}

func (h *Host) UsesAuthority(store DurableAuthority) bool {
	return h != nil && !nilValue(store) && reflect.TypeOf(store).Comparable() &&
		h.store == InstallationStore(store) && h.protection == ExecutionProtection(store) &&
		store.IsAuthority(h.admission, h.epoch)
}

func (s *Setup) UsesAuthority(store DurableAuthority) bool {
	return s != nil && s.host.UsesAuthority(store)
}

// InstallationFailure carries the original commit conclusion even if local reconciliation later fails.
type InstallationFailure struct {
	ID    InstallationID
	State CommitState
	Cause error
}

func (e *InstallationFailure) Error() string {
	return "frame: installation " + e.ID.String() + " (" + string(e.State) + "): " + e.Cause.Error()
}
func (e *InstallationFailure) Unwrap() error { return e.Cause }

func installationFailure(id InstallationID, state CommitState, cause error) error {
	if cause == nil {
		return nil
	}
	return &InstallationFailure{ID: id, State: state, Cause: cause}
}

// InstallationConclusion applies to Host installation operations. A preflight rejection never reached storage.
func InstallationConclusion(err error) CommitState {
	if err == nil {
		return CommitSucceeded
	}
	if failure, ok := errors.AsType[*InstallationFailure](err); ok {
		return failure.State
	}
	return CommitRejected
}

// Driver returns the registered implementation whose profile is to admit an archive entry.
func (h *Host) Driver(profile Profile) (Driver, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	driver, ok := h.drivers[profile]
	return driver.Driver, ok
}
