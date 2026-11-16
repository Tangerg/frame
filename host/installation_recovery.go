package host

import (
	"context"
	"fmt"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// Restore reads and projects the original durable authority under one maintenance
// admission. Caller-held records cannot authorize restoration. Retained business
// dependencies and execution protection must also be restored before the caller
// clears recovery; this operation neither starts resources nor clears that fence.
func (h *hostState) Restore(ctx context.Context, plugins []*Plugin) error {
	if h.durable == nil {
		return fmt.Errorf("%w: installation restoration requires durable authority", frame.ErrInvalid)
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
		work, err := h.lifetime.Enter(ctx)
		if err != nil {
			return err
		}
		defer work.Finish()
		h.mu.Lock()
		err = h.checkRestorationLocked()
		h.mu.Unlock()
		if err != nil {
			return err
		}
		records, err := h.readInstallationRestoration(work.Context())
		if err != nil {
			return err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := frame.ContextError(work.Context()); err != nil {
			return err
		}
		if err := h.checkRestorationLocked(); err != nil {
			return err
		}
		return h.restoreInstallationsLocked(records, definitions)
	})
}

func (h *hostState) checkRestorationLocked() error {
	if h.lifetime.IsClosing() {
		return frame.ErrClosed
	}
	if len(h.allInstances) != 0 || len(h.uses) != 0 {
		return ErrInUse
	}
	return nil
}

func (h *hostState) readInstallationRestoration(ctx context.Context) ([]InstallationRecord, error) {
	returned := false
	defer func() {
		if !returned {
			h.admission.RequireRecovery(fmt.Errorf("%w: installation restoration read interrupted", ErrRecoveryRequired))
		}
	}()
	records, err := h.durable.Installations(ctx)
	returned = true
	if err == nil {
		return records, nil
	}
	failure := fmt.Errorf("host: read installation restoration: %w", err)
	if onlyRequestCancellation(ctx, err) {
		return nil, failure
	}
	return nil, h.admission.untrusted(failure)
}

func (h *hostState) restoreInstallationsLocked(records []InstallationRecord, definitions map[framevalue.Digest]Plugin) error {
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
}

func (h *hostState) restoredInstallationsLocked(records []InstallationRecord, definitions map[framevalue.Digest]Plugin) (map[InstallationID]*installed, error) {
	if len(records) > h.limits.MaxInstallations {
		return nil, fmt.Errorf("%w: installations", frame.ErrCapacity)
	}
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
