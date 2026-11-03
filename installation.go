package frame

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	framevalue "github.com/Tangerg/frame/value"
)

type CommitState string

const (
	CommitSucceeded     CommitState = "committed"
	CommitRejected      CommitState = "notCommitted"
	CommitIndeterminate CommitState = "indeterminate"
)

type InstallationRecord struct {
	ID            InstallationID    `json:"id"`
	Release       framevalue.Digest `json:"release"`
	Configuration []byte            `json:"configuration"`
	Enabled       bool              `json:"enabled"`
	Units         []UnitDeclaration `json:"units"`
}

func (r InstallationRecord) Clone() InstallationRecord {
	r.Configuration = bytes.Clone(r.Configuration)
	r.Units = slices.Clone(r.Units)
	for i := range r.Units {
		r.Units[i] = r.Units[i].Clone()
	}
	return r
}

func (r InstallationRecord) Unit(id UnitID) UnitRef {
	return UnitRef{InstallationID: r.ID, Release: r.Release, UnitID: id}
}

func (r InstallationRecord) equal(other InstallationRecord) bool {
	return r.ID == other.ID && r.Release == other.Release && r.Enabled == other.Enabled &&
		bytes.Equal(r.Configuration, other.Configuration) && sameUnitDeclarations(r.Units, other.Units)
}

type InstallationChange struct {
	Before *InstallationRecord
	After  *InstallationRecord
}

// InstallationStore only encodes a transition already admitted by Host. A
// domain-specific use owner retains its own business rows and binding snapshots.
type InstallationStore interface {
	CommitInstallation(context.Context, InstallationChange) (CommitState, error)
	ConfirmInstallation(context.Context, InstallationID) error
	Installations(context.Context) ([]InstallationRecord, error)
}

type InstallationConfig struct {
	ID            InstallationID
	Plugin        *Plugin
	Configuration []byte
	Enabled       bool
}

func (c InstallationConfig) record() (InstallationRecord, error) {
	if err := c.ID.Validate(); err != nil {
		return InstallationRecord{}, err
	}
	if c.Plugin == nil {
		return InstallationRecord{}, ErrInvalid
	}
	return InstallationRecord{ID: c.ID, Release: c.Plugin.revision, Configuration: bytes.Clone(c.Configuration), Enabled: c.Enabled, Units: c.Plugin.Units()}, nil
}

// installed is the Host's projection of one committed installation record and
// the grant that fences everything started under it.
type installed struct {
	record InstallationRecord
	plugin *Plugin
	scope  ScopeGrant
}

// InstallationFailure carries the original commit conclusion even if local
// reconciliation later fails.
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

// InstallationConclusion applies to Host installation operations. A preflight
// rejection never reached storage.
func InstallationConclusion(err error) CommitState {
	if err == nil {
		return CommitSucceeded
	}
	if failure, ok := errors.AsType[*InstallationFailure](err); ok {
		return failure.State
	}
	return CommitRejected
}

func (h *Host) Installation(id InstallationID) (InstallationRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.installations[id]
	if entry == nil {
		return InstallationRecord{}, false
	}
	return entry.record.Clone(), true
}

func (h *Host) Install(ctx context.Context, config InstallationConfig) (InstallationRecord, error) {
	record, err := config.record()
	if err != nil {
		return InstallationRecord{}, err
	}
	state := CommitRejected
	err = h.admitInstallation(ctx, func(admitted context.Context) error {
		if err := h.checkInstallable(record); err != nil {
			return err
		}
		var commitErr error
		after := record.Clone()
		state, commitErr = h.commitInstallation(admitted, InstallationChange{After: &after})
		if state != CommitSucceeded {
			return commitErr
		}
		entry := &installed{record: record.Clone(), plugin: config.Plugin}
		if err := h.openInstallationScope(entry, commitErr); err != nil {
			return err
		}
		h.mu.Lock()
		h.installations[record.ID] = entry
		h.mu.Unlock()
		return errors.Join(commitErr, h.confirmInstallation(admitted, record.ID))
	})
	if state != CommitSucceeded {
		return InstallationRecord{}, installationFailure(config.ID, state, err)
	}
	return record.Clone(), installationFailure(config.ID, state, err)
}

func (h *Host) checkInstallable(record InstallationRecord) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.lifetime.isClosing():
		return ErrClosed
	case h.installations[record.ID] != nil:
		return fmt.Errorf("%w: installation already exists", ErrInvalid)
	case len(h.installations) >= h.limits.MaxInstallations:
		return fmt.Errorf("%w: installations", ErrCapacity)
	}
	if err := h.checkReleaseLocked(record); err != nil {
		return err
	}
	if err := h.validateUnitsLocked(record.Units); err != nil {
		return err
	}
	if record.Enabled {
		return h.admission.checkScopeCapacity(installationScope(record.ID))
	}
	return nil
}

// openInstallationScope grants a newly committed enabled installation. The
// commit is already a fact, so a local failure requires recovery.
func (h *Host) openInstallationScope(entry *installed, commitErr error) error {
	if !entry.record.Enabled {
		return nil
	}
	grant, err := h.admission.openScope(installationScope(entry.record.ID))
	if err != nil {
		h.admission.RequireRecovery(err)
		return errors.Join(ErrRecoveryRequired, commitErr, err)
	}
	entry.scope = grant
	return nil
}

// Update admits one compare-and-swap replacement of installation authority.
// Consumer configuration is prepared before this call; the original record is
// checked again under the same admission as retained-use and commit checks.
func (h *Host) Update(ctx context.Context, expected InstallationRecord, config InstallationConfig) error {
	next, err := config.record()
	if err != nil {
		return err
	}
	if next.ID != expected.ID {
		return ErrInvalid
	}
	return h.changeInstallation(ctx, next.ID, func(current *installed) (*installed, error) {
		if !current.record.equal(expected) {
			return nil, ErrStaleBinding
		}
		return &installed{record: next, plugin: config.Plugin}, nil
	})
}

func (h *Host) SetEnabled(ctx context.Context, id InstallationID, enabled bool) error {
	return h.changeInstallation(ctx, id, func(current *installed) (*installed, error) {
		record := current.record.Clone()
		record.Enabled = enabled
		return &installed{record: record, plugin: current.plugin}, nil
	})
}

func (h *Host) Replace(ctx context.Context, id InstallationID, plugin *Plugin, configuration []byte) error {
	if plugin == nil {
		return ErrInvalid
	}
	configuration = bytes.Clone(configuration)
	return h.changeInstallation(ctx, id, func(current *installed) (*installed, error) {
		record := InstallationRecord{ID: id, Release: plugin.revision, Configuration: configuration, Enabled: current.record.Enabled, Units: plugin.Units()}
		return &installed{record: record, plugin: plugin}, nil
	})
}

func (h *Host) Uninstall(ctx context.Context, id InstallationID) error {
	return h.changeInstallation(ctx, id, func(*installed) (*installed, error) { return nil, nil })
}

// installationTransition replaces one installed record; a nil after removes it.
type installationTransition struct {
	before *installed
	after  *installed
}

// destructive transitions invalidate every binding to the original release or
// configuration, so they require that no business owner still retains it.
func (t installationTransition) destructive() bool {
	return t.after == nil || t.after.record.Release != t.before.record.Release || !bytes.Equal(t.after.record.Configuration, t.before.record.Configuration)
}

func (t installationTransition) change() InstallationChange {
	before := t.before.record.Clone()
	change := InstallationChange{Before: &before}
	if t.after != nil {
		after := t.after.record.Clone()
		change.After = &after
	}
	return change
}

func (h *Host) changeInstallation(ctx context.Context, id InstallationID, prepare func(*installed) (*installed, error)) error {
	if err := id.Validate(); err != nil {
		return err
	}
	state := CommitRejected
	var retired []*Instance
	err := h.admitInstallation(ctx, func(admitted context.Context) error {
		transition, err := h.planInstallationTransition(id, prepare)
		if err != nil || transition == nil {
			return err
		}
		next := transition.after
		if next != nil && next.record.Enabled {
			if err := h.admission.checkScopeCapacity(installationScope(id)); err != nil {
				return err
			}
		}
		if transition.destructive() {
			if err := h.admission.CheckUnused(admitted, id); err != nil {
				return err
			}
		}
		var commitErr error
		state, commitErr = h.commitInstallation(admitted, transition.change())
		if state != CommitSucceeded {
			return commitErr
		}
		h.admission.fenceScope(installationScope(id))
		if next != nil {
			if err := h.openInstallationScope(next, commitErr); err != nil {
				return err
			}
		}
		retired = h.applyInstallationTransition(id, next)
		return errors.Join(commitErr, h.confirmInstallation(admitted, id))
	})
	for _, instance := range retired {
		instance.beginRetire()
	}
	return installationFailure(id, state, err)
}

func (h *Host) planInstallationTransition(id InstallationID, prepare func(*installed) (*installed, error)) (*installationTransition, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() {
		return nil, ErrClosed
	}
	current := h.installations[id]
	if current == nil {
		return nil, ErrUnavailable
	}
	next, err := prepare(current)
	if err != nil {
		return nil, err
	}
	if next != nil && current.record.equal(next.record) {
		return nil, nil
	}
	transition := &installationTransition{before: current, after: next}
	if transition.destructive() && h.usesInstallationLocked(id) {
		return nil, ErrInUse
	}
	if next == nil {
		return transition, nil
	}
	if err := h.checkReleaseLocked(next.record); err != nil {
		return nil, err
	}
	if err := h.validateUnitsLocked(next.record.Units); err != nil {
		return nil, err
	}
	return transition, nil
}

func (h *Host) usesInstallationLocked(id InstallationID) bool {
	for _, use := range h.uses {
		if use.binding.requiresInstallation(id) {
			return true
		}
	}
	return false
}

func (h *Host) applyInstallationTransition(id InstallationID, next *installed) []*Instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	if next == nil {
		delete(h.installations, id)
	} else {
		h.installations[id] = next
	}
	var retired []*Instance
	for _, instance := range h.instances {
		if instance.snapshot.requiresInstallation(id) {
			retired = append(retired, instance)
		}
	}
	for _, instance := range retired {
		h.retireInstanceLocked(instance)
	}
	return retired
}

// The close outcome must include control transactions already admitted, even
// if they have yet to create a resource or to publish their local projection.
func (h *Host) admitInstallation(ctx context.Context, apply func(context.Context) error) error {
	return h.admission.Admit(ctx, func(admitted context.Context) error {
		work, err := h.lifetime.Enter(admitted)
		if err != nil {
			return err
		}
		defer work.Finish()
		return apply(admitted)
	})
}

func (h *Host) commitInstallation(ctx context.Context, change InstallationChange) (CommitState, error) {
	if h.store == nil {
		return CommitSucceeded, nil
	}
	state, err := h.store.CommitInstallation(ctx, change)
	switch state {
	case CommitSucceeded:
		return CommitSucceeded, err
	case CommitRejected:
		if err == nil {
			err = fmt.Errorf("%w: installation commit rejected", ErrUnavailable)
		}
		return CommitRejected, err
	default:
		h.admission.RequireRecovery(errors.Join(ErrRecoveryRequired, err))
		return CommitIndeterminate, errors.Join(ErrRecoveryRequired, err)
	}
}

func (h *Host) confirmInstallation(ctx context.Context, id InstallationID) error {
	if h.store == nil {
		return nil
	}
	if err := h.store.ConfirmInstallation(ctx, id); err != nil {
		h.admission.RequireRecovery(err)
		return errors.Join(ErrRecoveryRequired, err)
	}
	return nil
}

// A release identifies immutable declarations, so every installation of the
// same release must declare exactly the same units.
func (h *Host) checkReleaseLocked(record InstallationRecord) error {
	for _, original := range h.installations {
		if original.record.Release == record.Release && !sameUnitDeclarations(original.record.Units, record.Units) {
			return fmt.Errorf("%w: release declarations changed", ErrContractMismatch)
		}
	}
	return nil
}

func (h *Host) validateUnitsLocked(units []UnitDeclaration) error {
	for _, unit := range units {
		if unit.Profile != NativeGo && h.drivers[unit.Profile].Driver == nil {
			return fmt.Errorf("%w: execution profile %s", ErrUnavailable, unit.Profile)
		}
		for _, export := range unit.Exports {
			if h.contracts.lookup(export.Contract) == nil {
				return fmt.Errorf("%w: export contract %s not registered", ErrContractMismatch, export.Contract)
			}
		}
		for _, required := range unit.Imports {
			if h.contracts.lookup(required.Contract) == nil {
				return fmt.Errorf("%w: import contract %s not registered", ErrContractMismatch, required.Contract)
			}
		}
	}
	return nil
}

// Restore projects committed installation records without starting resources or
// reopening the global recovery barrier. The caller must also restore retained
// business dependencies and execution protection before finishing recovery.
func (h *Host) Restore(ctx context.Context, records []InstallationRecord, plugins []*Plugin) error {
	if len(records) > h.limits.MaxInstallations {
		return fmt.Errorf("%w: installations", ErrCapacity)
	}
	definitions := make(map[framevalue.Digest]*Plugin, len(plugins))
	for _, plugin := range plugins {
		if plugin == nil {
			return ErrInvalid
		}
		if existing := definitions[plugin.revision]; existing != nil && !sameUnitDeclarations(existing.Units(), plugin.Units()) {
			return ErrContractMismatch
		}
		definitions[plugin.revision] = plugin
	}
	return h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.lifetime.isClosing() {
			return ErrClosed
		}
		if len(h.allInstances) != 0 || len(h.uses) != 0 {
			return ErrInUse
		}
		restored, err := h.restoredInstallationsLocked(records, definitions)
		if err != nil {
			return err
		}
		for _, entry := range restored {
			if !entry.record.Enabled {
				continue
			}
			if entry.scope, err = h.admission.restoreScope(installationScope(entry.record.ID)); err != nil {
				return err
			}
		}
		for id := range h.installations {
			if entry := restored[id]; entry == nil || !entry.record.Enabled {
				h.admission.fenceScope(installationScope(id))
			}
		}
		h.installations = restored
		return nil
	})
}

func (h *Host) restoredInstallationsLocked(records []InstallationRecord, definitions map[framevalue.Digest]*Plugin) (map[InstallationID]*installed, error) {
	restored := make(map[InstallationID]*installed, len(records))
	var scopes []scopeKey
	for _, record := range records {
		if err := record.ID.Validate(); err != nil {
			return nil, err
		}
		if err := h.checkReleaseLocked(record); err != nil {
			return nil, err
		}
		plugin := definitions[record.Release]
		if plugin == nil {
			return nil, fmt.Errorf("%w: definition for retained release", ErrUnavailable)
		}
		if restored[record.ID] != nil {
			return nil, ErrInvalid
		}
		if err := h.validateUnitsLocked(record.Units); err != nil {
			return nil, err
		}
		if !sameUnitDeclarations(record.Units, plugin.Units()) {
			return nil, ErrContractMismatch
		}
		restored[record.ID] = &installed{record: record.Clone(), plugin: plugin}
		if record.Enabled {
			scopes = append(scopes, installationScope(record.ID))
		}
	}
	return restored, h.admission.checkScopeCapacity(scopes...)
}

func sameUnitDeclarations(a, b []UnitDeclaration) bool {
	return slices.EqualFunc(a, b, UnitDeclaration.equal)
}
