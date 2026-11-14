package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type CommitState string

const (
	CommitSucceeded     CommitState = "committed"
	CommitRejected      CommitState = "notCommitted"
	CommitIndeterminate CommitState = "indeterminate"
)

// InstallationRecord is the durable projection of one installation. The
// release digest names its declarations; their single owner is the Plugin
// supplied again when the record is restored.
type InstallationRecord struct {
	ID            InstallationID    `json:"id"`
	Release       framevalue.Digest `json:"release"`
	Configuration []byte            `json:"configuration"`
	Enabled       bool              `json:"enabled"`
}

func (r InstallationRecord) Clone() InstallationRecord {
	r.Configuration = bytes.Clone(r.Configuration)
	return r
}

func (r InstallationRecord) Unit(id UnitID) UnitRef {
	return UnitRef{InstallationID: r.ID, Release: r.Release, UnitID: id}
}

func (r InstallationRecord) equal(other InstallationRecord) bool {
	return r.ID == other.ID && r.Release == other.Release && r.Enabled == other.Enabled &&
		bytes.Equal(r.Configuration, other.Configuration)
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

func (c InstallationConfig) capture() (*installed, error) {
	if err := c.ID.Validate(); err != nil {
		return nil, err
	}
	if c.Plugin == nil {
		return nil, frame.ErrInvalid
	}
	plugin := *c.Plugin
	if err := plugin.revision.Validate(); err != nil {
		return nil, err
	}
	return &installed{
		plugin: plugin,
		record: InstallationRecord{ID: c.ID, Release: plugin.revision, Configuration: bytes.Clone(c.Configuration), Enabled: c.Enabled},
	}, nil
}

// The captured plugin has no external mutable handle. Its record is a durable
// projection, and its scope fences everything started under this installation.
type installed struct {
	record InstallationRecord
	plugin Plugin
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
	return "host: installation " + e.ID.String() + " (" + string(e.State) + "): " + e.Cause.Error()
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

func (h *hostState) Installation(id InstallationID) (InstallationRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.installations[id]
	if entry == nil {
		return InstallationRecord{}, false
	}
	return entry.record.Clone(), true
}

func (h *hostState) Install(ctx context.Context, config InstallationConfig) (InstallationRecord, error) {
	entry, err := config.capture()
	if err != nil {
		return InstallationRecord{}, err
	}
	state, err := h.mutateInstallation(ctx, entry.record.ID, func(current *installed) (*installed, error) {
		if current != nil {
			return nil, fmt.Errorf("%w: installation already exists", frame.ErrInvalid)
		}
		return entry, nil
	})
	if state != CommitSucceeded {
		return InstallationRecord{}, installationFailure(config.ID, state, err)
	}
	return entry.record.Clone(), installationFailure(config.ID, state, err)
}

// projectInstallationScope grants a newly committed enabled installation. The
// commit is already a fact, so a local failure requires recovery.
func (h *hostState) projectInstallationScope(transition *installationTransition, commitErr error) error {
	entry := transition.after
	key := installationScope(transition.id())
	if entry == nil || !entry.record.Enabled {
		h.admission.fenceScope(key)
		return nil
	}
	grant, err := h.admission.restoreScope(key, transition.admittedScope)
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
func (h *hostState) Update(ctx context.Context, expected InstallationRecord, config InstallationConfig) error {
	next, err := config.capture()
	if err != nil {
		return err
	}
	if next.record.ID != expected.ID {
		return frame.ErrInvalid
	}
	return h.changeInstallation(ctx, next.record.ID, func(current *installed) (*installed, error) {
		if !current.record.equal(expected) {
			return nil, ErrStaleBinding
		}
		return next, nil
	})
}

func (h *hostState) SetEnabled(ctx context.Context, id InstallationID, enabled bool) error {
	return h.changeInstallation(ctx, id, func(current *installed) (*installed, error) {
		record := current.record.Clone()
		record.Enabled = enabled
		return &installed{record: record, plugin: current.plugin}, nil
	})
}

func (h *hostState) Replace(ctx context.Context, id InstallationID, plugin *Plugin, configuration []byte) error {
	next, err := (InstallationConfig{ID: id, Plugin: plugin, Configuration: configuration}).capture()
	if err != nil {
		return err
	}
	return h.changeInstallation(ctx, id, func(current *installed) (*installed, error) {
		next.record.Enabled = current.record.Enabled
		return next, nil
	})
}

func (h *hostState) Uninstall(ctx context.Context, id InstallationID) error {
	return h.changeInstallation(ctx, id, func(*installed) (*installed, error) { return nil, nil })
}

// An admitted transition retains its exact intent and scope revision until the
// durable outcome has been projected. Recovery cannot infer this intent from
// Enabled alone or reopen a generation fenced after the original admission.
type installationTransition struct {
	before        *installed
	after         *installed
	admittedScope *scopeState
}

func (t *installationTransition) id() InstallationID {
	if t.after != nil {
		return t.after.record.ID
	}
	return t.before.record.ID
}

// destructive transitions invalidate every binding to the original release or
// configuration, so they require that no business owner still retains it.
func (t installationTransition) destructive() bool {
	return t.before != nil && (t.after == nil || t.after.record.Release != t.before.record.Release || !bytes.Equal(t.after.record.Configuration, t.before.record.Configuration))
}

func (t installationTransition) change() InstallationChange {
	var change InstallationChange
	if t.before != nil {
		before := t.before.record.Clone()
		change.Before = &before
	}
	if t.after != nil {
		after := t.after.record.Clone()
		change.After = &after
	}
	return change
}

func (h *hostState) changeInstallation(ctx context.Context, id InstallationID, prepare func(*installed) (*installed, error)) error {
	state, err := h.mutateInstallation(ctx, id, func(current *installed) (*installed, error) {
		if current == nil {
			return nil, ErrUnavailable
		}
		return prepare(current)
	})
	return installationFailure(id, state, err)
}

func (h *hostState) mutateInstallation(ctx context.Context, id InstallationID, prepare func(*installed) (*installed, error)) (CommitState, error) {
	if err := id.Validate(); err != nil {
		return CommitRejected, err
	}
	state := CommitRejected
	var retired []*unitInstance
	// Confirmation can interrupt after withdrawal; the original retirement must still start.
	defer func() {
		for _, instance := range retired {
			instance.beginRetire()
		}
	}()
	err := h.admitInstallation(ctx, func(admitted context.Context) error {
		transition, err := h.planInstallationTransition(id, prepare)
		if err != nil {
			return err
		}
		if transition == nil {
			state = CommitSucceeded
			return nil
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
		state, commitErr = h.commitInstallation(admitted, transition)
		if state != CommitSucceeded {
			return commitErr
		}
		if err := h.projectInstallationScope(transition, commitErr); err != nil {
			return err
		}
		retired = h.applyInstallationTransition(id, next)
		return errors.Join(commitErr, h.confirmInstallation(admitted, id))
	})
	return state, err
}

func (h *hostState) planInstallationTransition(id InstallationID, prepare func(*installed) (*installed, error)) (*installationTransition, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.IsClosing() {
		return nil, frame.ErrClosed
	}
	current := h.installations[id]
	next, err := prepare(current)
	if err != nil {
		return nil, err
	}
	if next != nil {
		if err := h.checkReleaseLocked(next.plugin); err != nil {
			return nil, err
		}
	}
	if current != nil && next != nil && current.record.equal(next.record) {
		return nil, nil
	}
	if current == nil && len(h.installations) >= h.limits.MaxInstallations {
		return nil, fmt.Errorf("%w: installations", frame.ErrCapacity)
	}
	transition := &installationTransition{before: current, after: next}
	if transition.destructive() && h.usesInstallationLocked(id) {
		return nil, ErrInUse
	}
	if next == nil {
		return transition, nil
	}
	if err := h.validateUnitsLocked(next.plugin.Units()); err != nil {
		return nil, err
	}
	return transition, nil
}

func (h *hostState) usesInstallationLocked(id InstallationID) bool {
	for _, use := range h.uses {
		if use.binding.requiresInstallation(id) {
			return true
		}
	}
	return false
}

func (h *hostState) applyInstallationTransition(id InstallationID, next *installed) []*unitInstance {
	h.mu.Lock()
	defer h.mu.Unlock()
	if next == nil {
		delete(h.installations, id)
	} else {
		h.installations[id] = next
	}
	h.installationTransition = nil
	var retired []*unitInstance
	for _, instance := range h.instances {
		if instance.snapshot.requiresInstallation(id) {
			retired = append(retired, instance)
		}
	}
	for _, instance := range retired {
		instance.withdrawLocked()
	}
	return retired
}

// The close outcome must include control transactions already admitted, even
// if they have yet to create a resource or to publish their local projection.
func (h *hostState) admitInstallation(ctx context.Context, apply func(context.Context) error) error {
	return h.admission.Admit(ctx, func(admitted context.Context) error {
		h.mu.Lock()
		pending := h.installationTransition != nil
		h.mu.Unlock()
		if pending {
			err := fmt.Errorf("%w: original installation transition requires restoration", ErrRecoveryRequired)
			h.admission.RequireRecovery(err)
			return err
		}
		work, err := h.lifetime.Enter(admitted)
		if err != nil {
			return err
		}
		defer work.Finish()
		return apply(admitted)
	})
}

func (h *hostState) commitInstallation(ctx context.Context, transition *installationTransition) (CommitState, error) {
	if err := h.admission.CheckAdmitted(ctx); err != nil {
		return CommitRejected, err
	}
	if err := h.admission.RecoveryError(); err != nil {
		return CommitRejected, err
	}
	transition.admittedScope = h.admission.captureScope(installationScope(transition.id()))
	h.mu.Lock()
	h.installationTransition = transition
	h.mu.Unlock()
	if h.durable == nil {
		return CommitSucceeded, nil
	}
	returned := false
	defer func() {
		if !returned {
			h.admission.RequireRecovery(fmt.Errorf("%w: installation commit interrupted", ErrRecoveryRequired))
		}
	}()
	state, err := h.durable.CommitInstallation(ctx, transition.change())
	returned = true
	switch state {
	case CommitSucceeded:
		return CommitSucceeded, err
	case CommitRejected:
		h.mu.Lock()
		h.installationTransition = nil
		h.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("%w: installation commit rejected", ErrUnavailable)
		}
		return CommitRejected, err
	default:
		h.admission.RequireRecovery(errors.Join(ErrRecoveryRequired, err))
		return CommitIndeterminate, errors.Join(ErrRecoveryRequired, err)
	}
}

func (h *hostState) confirmInstallation(ctx context.Context, id InstallationID) error {
	if h.durable == nil {
		return nil
	}
	returned := false
	defer func() {
		if !returned {
			h.admission.RequireRecovery(fmt.Errorf("%w: installation confirmation interrupted", ErrRecoveryRequired))
		}
	}()
	err := h.durable.ConfirmInstallation(ctx, id)
	returned = true
	if err != nil {
		h.admission.RequireRecovery(err)
		return errors.Join(ErrRecoveryRequired, err)
	}
	return nil
}

// A caller supplies the release digest of a native Plugin, so two Plugins can
// claim one release. Every installation of a release must declare the same units.
func (h *hostState) checkReleaseLocked(plugin Plugin) error {
	for _, original := range h.installations {
		if original.plugin.revision == plugin.revision && !sameUnitDeclarations(original.plugin.Units(), plugin.Units()) {
			return fmt.Errorf("%w: release declarations changed", frame.ErrContractMismatch)
		}
	}
	return nil
}

func (h *hostState) validateUnitsLocked(units []UnitDeclaration) error {
	for _, unit := range units {
		if unit.Profile != NativeGo && h.drivers[unit.Profile].Driver == nil {
			return fmt.Errorf("%w: execution profile %s", ErrUnavailable, unit.Profile)
		}
		for _, export := range unit.Exports {
			if h.contracts.lookup(export.Contract) == nil {
				return fmt.Errorf("%w: export contract %s not registered", frame.ErrContractMismatch, export.Contract)
			}
		}
		for _, required := range unit.Imports {
			if h.contracts.lookup(required.Contract) == nil {
				return fmt.Errorf("%w: import contract %s not registered", frame.ErrContractMismatch, required.Contract)
			}
		}
	}
	return nil
}

// Restore projects committed installation records without starting resources or
// reopening the global recovery barrier. The caller must also restore retained
// business dependencies and execution protection before finishing recovery.
func (h *hostState) Restore(ctx context.Context, records []InstallationRecord, plugins []*Plugin) error {
	if len(records) > h.limits.MaxInstallations {
		return fmt.Errorf("%w: installations", frame.ErrCapacity)
	}
	definitions := make(map[framevalue.Digest]Plugin, len(plugins))
	for _, plugin := range plugins {
		if plugin == nil {
			return frame.ErrInvalid
		}
		captured := *plugin
		if err := captured.revision.Validate(); err != nil {
			return err
		}
		if existing, found := definitions[captured.revision]; found && !sameUnitDeclarations(existing.Units(), captured.Units()) {
			return frame.ErrContractMismatch
		}
		definitions[captured.revision] = captured
	}
	return h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.lifetime.IsClosing() {
			return frame.ErrClosed
		}
		if len(h.allInstances) != 0 || len(h.uses) != 0 {
			return ErrInUse
		}
		restored, err := h.restoredInstallationsLocked(records, definitions)
		if err != nil {
			return err
		}
		transition := h.installationTransition
		if transition != nil && !transition.acceptsRestoration(restored[transition.id()]) {
			return ErrStaleBinding
		}
		for _, entry := range restored {
			if !entry.record.Enabled {
				continue
			}
			var admittedScope *scopeState
			if transition != nil && transition.after != nil && transition.after.record.equal(entry.record) {
				admittedScope = transition.admittedScope
			}
			if entry.scope, err = h.admission.restoreScope(installationScope(entry.record.ID), admittedScope); err != nil {
				return err
			}
		}
		for id := range h.installations {
			if entry := restored[id]; entry == nil || !entry.record.Enabled {
				h.admission.fenceScope(installationScope(id))
			}
		}
		h.installations = restored
		h.installationTransition = nil
		return nil
	})
}

func (t *installationTransition) acceptsRestoration(restored *installed) bool {
	for _, expected := range []*installed{t.before, t.after} {
		if restored == nil && expected == nil || restored != nil && expected != nil && restored.record.equal(expected.record) {
			return true
		}
	}
	return false
}

func (h *hostState) restoredInstallationsLocked(records []InstallationRecord, definitions map[framevalue.Digest]Plugin) (map[InstallationID]*installed, error) {
	restored := make(map[InstallationID]*installed, len(records))
	var scopes []scopeKey
	for _, record := range records {
		if err := record.ID.Validate(); err != nil {
			return nil, err
		}
		plugin, found := definitions[record.Release]
		if !found {
			return nil, fmt.Errorf("%w: definition for retained release", ErrUnavailable)
		}
		if err := h.checkReleaseLocked(plugin); err != nil {
			return nil, err
		}
		if restored[record.ID] != nil {
			return nil, frame.ErrInvalid
		}
		if err := h.validateUnitsLocked(plugin.Units()); err != nil {
			return nil, err
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
