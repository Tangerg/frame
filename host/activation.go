package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// activationBinding yields the exact graph an activation realizes, in provider
// order. It runs under the activation's admission and locks Host.mu itself.
type activationBinding func(admitted context.Context) (BindingSnapshot, []bindingRead, error)

// Canceling the caller stops its wait; accepted attempts belong to the Host.
func (h *hostState) Activate(ctx context.Context, roots ...ExportRef) (BindingSnapshot, error) {
	return h.ActivateIn(ctx, "", roots...)
}

func (h *hostState) ActivateIn(ctx context.Context, realm RealmID, roots ...ExportRef) (BindingSnapshot, error) {
	plan, err := h.activate(ctx, realm, ScopeGrant{}, h.resolvedBinding(realm, roots))
	if err != nil {
		return BindingSnapshot{}, err
	}
	return plan.binding.Clone(), nil
}

// ActivatePlan admits a declaration plan and rechecks its original candidate
// reads through preparation. Recovery reads an accepted binding from its business owner.
func (h *hostState) ActivatePlan(ctx context.Context, realm RealmID, plan BindingPlan) (BindingSnapshot, error) {
	if plan.bindingPlan == nil || plan.kind != declarationPlan || plan.realm != realm {
		return BindingSnapshot{}, frame.ErrInvalid
	}
	prepared, err := h.activate(ctx, realm, ScopeGrant{}, h.plannedBinding(plan.bindingPlan))
	if err != nil {
		return BindingSnapshot{}, err
	}
	return prepared.binding.Clone(), nil
}

func (h *hostState) ActivateUnit(ctx context.Context, realm RealmID, root UnitRef) (BindingSnapshot, error) {
	plan, err := h.PlanUnit(ctx, realm, root, nil)
	if err != nil {
		return BindingSnapshot{}, err
	}
	return h.ActivatePlan(ctx, realm, plan)
}

func (h *hostState) resolvedBinding(realm RealmID, roots []ExportRef) activationBinding {
	return func(context.Context) (BindingSnapshot, []bindingRead, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		plan, err := h.resolveLocked(roots, nil, realm, nil)
		if err != nil {
			return BindingSnapshot{}, nil, err
		}
		return plan.binding, plan.bindingReads, nil
	}
}

func (h *hostState) plannedBinding(plan *bindingPlan) activationBinding {
	return func(context.Context) (BindingSnapshot, []bindingRead, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := h.validatePlanLocked(plan); err != nil {
			return BindingSnapshot{}, nil, err
		}
		return plan.binding.Clone(), plan.bindingReads, nil
	}
}

// acceptedBinding restores the graph its business owner committed. It never
// resolves providers again; it only checks that graph against the installations.
func (h *hostState) acceptedBinding(recordID string, grant ScopeGrant) activationBinding {
	return func(admitted context.Context) (BindingSnapshot, []bindingRead, error) {
		accepted, err := h.readAcceptedBinding(admitted, recordID, grant)
		if err != nil {
			return BindingSnapshot{}, nil, err
		}
		if err := h.ValidateBindingUnder(admitted, accepted); err != nil {
			return BindingSnapshot{}, nil, err
		}
		binding, err := reachableBinding(accepted, accepted.Roots, accepted.UnitRoots)
		return binding, nil, err
	}
}

func (h *hostState) activate(ctx context.Context, realm RealmID, grant ScopeGrant, bind activationBinding) (*bindingPlan, error) {
	var binding BindingSnapshot
	var created []*unitInstance
	var planned map[UnitRef]*unitInstance
	err := h.admission.Admit(ctx, func(admitted context.Context) error {
		if grant.issued() {
			if err := h.admission.ValidateScope(grant); err != nil {
				return err
			}
		}
		var reads []bindingRead
		var err error
		binding, reads, err = bind(admitted)
		if err != nil {
			return err
		}
		if grant.issued() {
			if err := h.admission.ValidateScope(grant); err != nil {
				return err
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.lifetime.IsClosing() {
			return frame.ErrClosed
		}
		planned = make(map[UnitRef]*unitInstance, len(binding.Units))
		for _, bound := range binding.Units {
			instance, isNew, err := h.planInstanceLocked(binding, bound, realm, reads, planned, grant)
			if err != nil {
				return err
			}
			planned[bound.Source] = instance
			if isNew {
				created = append(created, instance)
			}
		}
		return nil
	})
	if err != nil {
		h.abandon(created, err)
		return nil, err
	}
	for _, instance := range created {
		go instance.prepare()
	}
	if err := h.awaitReady(ctx, binding, planned); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.capturePreparedLocked(binding, realm, planned)
}

// Preparation hands off its original instances. Looking them up again by unit
// would allow a successor with the same binding to replace that preparation.
func (h *hostState) capturePreparedLocked(binding BindingSnapshot, realm RealmID, instances map[UnitRef]*unitInstance) (*bindingPlan, error) {
	for _, bound := range binding.Units {
		if err := instances[bound.Source].validateServingLocked(); err != nil {
			return nil, err
		}
	}
	plan := &bindingPlan{kind: instancePlan, owner: h.identity, realm: realm, binding: binding.Clone()}
	for _, source := range binding.Roots {
		instance := instances[source.Unit()]
		export, err := instance.exports.bound(source)
		if err != nil {
			return nil, err
		}
		plan.members = append(plan.members, export.snapshot())
	}
	for _, source := range binding.UnitRoots {
		instance := instances[source]
		plan.unitMembers = append(plan.unitMembers, unitMember{source: source, instanceID: instance.id})
	}
	return plan, nil
}

func (h *hostState) abandon(created []*unitInstance, cause error) {
	h.mu.Lock()
	for _, instance := range created {
		instance.preparation.complete(cause)
		instance.withdrawLocked()
	}
	h.mu.Unlock()
	for _, instance := range created {
		instance.beginRetire()
	}
}

func (h *hostState) planInstanceLocked(binding BindingSnapshot, bound UnitBinding, realm RealmID, reads []bindingRead, planned map[UnitRef]*unitInstance, grant ScopeGrant) (*unitInstance, bool, error) {
	unit, installation, err := h.unitLocked(bound.Source)
	if err != nil {
		return nil, false, err
	}
	realm, err = h.realmLocked(unit.Declaration, realm)
	if err != nil {
		return nil, false, err
	}
	if err := h.admission.ValidateScope(installation.scope); err != nil {
		return nil, false, err
	}
	snapshot, identity, err := setupBinding(binding, declaredExports(bound.Source, unit.Declaration), bound.Source)
	if err != nil {
		return nil, false, err
	}
	key := instanceKey{source: bound.Source, realm: realm}
	if current := h.instances[key]; current != nil {
		if current.startGrant.issued() && current.startGrant != grant {
			return nil, false, ErrFenced
		}
		if current.identity != identity {
			return nil, false, fmt.Errorf("%w: unit already has another setup binding", ErrInUse)
		}
		return current, false, nil
	}
	if h.overlappingInstanceLocked(key) {
		return nil, false, fmt.Errorf("%w: original unit has not proved resource release", ErrInUse)
	}
	h.nextGeneration++
	lifetime := h.newLifetime(context.WithoutCancel(h.lifetime.Context()))
	instance := &unitInstance{
		host:          h,
		id:            rand.Text(),
		generation:    h.nextGeneration,
		scope:         installation.scope,
		startGrant:    grant,
		realm:         realm,
		unit:          unit.Declaration.Clone(),
		binding:       bound.Clone(),
		snapshot:      snapshot,
		identity:      identity,
		configuration: bytes.Clone(installation.record.Configuration),
		setup:         unit.Setup,
		lifetime:      lifetime,
		preparation:   newUnitPreparation(bound.Source, reads),
		retirement:    newInstanceRetirement(lifetime),
	}
	instance.exports = newUnitExports(&h.catalog, instance, h.limits.MaxPublicationOperations)
	providers, err := instance.holdProvidersLocked(planned)
	if err == nil {
		err = h.registerInstanceLocked(instance, providers)
	}
	if err != nil {
		instance.releaseProvidersLocked()
		return nil, false, err
	}
	return instance, true, nil
}

// Diagnostic forwarding may outlive the actual release. Only the original
// Lifetime's proof can conclude whether an earlier execution still overlaps.
func (h *hostState) overlappingInstanceLocked(key instanceKey) bool {
	for _, instance := range h.allInstances {
		source := instance.binding.Source
		if source.InstallationID == key.source.InstallationID && source.UnitID == key.source.UnitID && instance.realm == key.realm {
			if outcome, _ := instance.lifetime.CloseOutcome(); !outcome.Released {
				return true
			}
		}
	}
	return false
}

func declaredExports(source UnitRef, unit UnitDeclaration) []ExportRef {
	exports := make([]ExportRef, 0, len(unit.Exports))
	for _, export := range unit.Exports {
		exports = append(exports, ExportRef{InstallationID: source.InstallationID, Release: source.Release, UnitID: source.UnitID, Export: export.Name, Contract: export.Contract})
	}
	return exports
}

// setupBinding projects the exact graph one unit's setup depends on. The
// consumer's selection rule belongs to its capture, never to setup identity.
func setupBinding(graph BindingSnapshot, exports []ExportRef, unit UnitRef) (BindingSnapshot, framevalue.Digest, error) {
	binding, err := reachableBinding(graph, exports, []UnitRef{unit})
	if err != nil {
		return BindingSnapshot{}, framevalue.Digest{}, err
	}
	binding.Rule = ""
	identity, err := binding.Identity()
	return binding, identity, err
}

func (h *hostState) awaitReady(ctx context.Context, binding BindingSnapshot, instances map[UnitRef]*unitInstance) error {
	for _, bound := range binding.Units {
		instance := instances[bound.Source]
		select {
		case <-instance.preparation.done:
		case <-ctx.Done():
			return frame.ContextError(ctx)
		}
		h.mu.Lock()
		err, current := instance.preparation.failure, instance.isCurrentLocked()
		h.mu.Unlock()
		if err != nil {
			return err
		}
		if !current {
			return ErrStaleBinding
		}
	}
	return nil
}
