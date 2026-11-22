package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"maps"

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
	declared, ok := plan.plan.(*declaredPlan)
	if !ok || declared.realm != realm {
		return BindingSnapshot{}, frame.ErrInvalid
	}
	prepared, err := h.activate(ctx, realm, ScopeGrant{}, h.plannedBinding(declared))
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
		return plan.binding, plan.reads, nil
	}
}

func (h *hostState) plannedBinding(plan *declaredPlan) activationBinding {
	return func(context.Context) (BindingSnapshot, []bindingRead, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := h.validatePlanLocked(plan); err != nil {
			return BindingSnapshot{}, nil, err
		}
		return plan.binding.Clone(), plan.reads, nil
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
		binding, err := accepted.reachable(accepted.Roots, accepted.UnitRoots)
		return binding, nil, err
	}
}

// Only this preparation dispatches its newly admitted instances. Reused
// instances keep their original workers, and handoff cannot select successors.
type bindingPreparation struct {
	host      *hostState
	realm     RealmID
	binding   BindingSnapshot
	instances map[UnitRef]*unitInstance
	created   []*unitInstance
}

func (h *hostState) activate(ctx context.Context, realm RealmID, grant ScopeGrant, bind activationBinding) (*capturedPlan, error) {
	var preparation *bindingPreparation
	err := h.admission.Admit(ctx, func(admitted context.Context) error {
		var err error
		preparation, err = h.planPreparation(admitted, realm, grant, bind)
		return err
	})
	if err != nil {
		return nil, err
	}
	return preparation.prepare(ctx)
}

func (h *hostState) planPreparation(ctx context.Context, realm RealmID, grant ScopeGrant, bind activationBinding) (prepared *bindingPreparation, err error) {
	if err := h.admission.CheckAdmitted(ctx); err != nil {
		return nil, err
	}
	if grant.Valid() {
		if err := h.admission.ValidateScope(grant); err != nil {
			return nil, err
		}
	}
	binding, reads, err := bind(ctx)
	if err != nil {
		return nil, err
	}
	if grant.Valid() {
		if err := h.admission.ValidateScope(grant); err != nil {
			return nil, err
		}
	}
	preparation := &bindingPreparation{host: h, realm: realm, binding: binding, instances: make(map[UnitRef]*unitInstance, len(binding.Units))}
	defer func() {
		if prepared == nil {
			if err == nil {
				err = fmt.Errorf("%w: binding preparation planning interrupted", ErrUnavailable)
			}
			preparation.abandon(err)
		}
	}()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.IsClosing() {
		return nil, frame.ErrClosed
	}
	for _, bound := range binding.Units {
		instance, isNew, err := h.planInstanceLocked(binding, bound, realm, reads, preparation.instances, grant)
		if err != nil {
			return nil, err
		}
		preparation.instances[bound.Source] = instance
		if isNew {
			preparation.created = append(preparation.created, instance)
		}
	}
	return preparation, nil
}

func (p *bindingPreparation) prepare(ctx context.Context) (*capturedPlan, error) {
	for _, instance := range p.created {
		go instance.prepare()
	}
	h := p.host
	for _, bound := range p.binding.Units {
		instance := p.instances[bound.Source]
		select {
		case <-instance.preparation.done:
		case <-ctx.Done():
			return nil, frame.ContextError(ctx)
		}
		h.mu.Lock()
		err, current := instance.preparation.failure, instance.isCurrentLocked()
		h.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if !current {
			return nil, ErrStaleBinding
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return p.captureLocked()
}

func (p *bindingPreparation) claimStart(root UnitRef, grant ScopeGrant) (RunningUnit, error) {
	h := p.host
	h.mu.Lock()
	instance := p.instances[root]
	var running RunningUnit
	var err error
	if instance == nil {
		err = frame.ErrInvalid
	} else {
		running, err = instance.claimStartLocked(grant)
	}
	h.mu.Unlock()
	if err != nil {
		p.abandon(err)
	}
	return running, err
}

func (p *bindingPreparation) checkUseOwner(owner frame.Lifetime) error {
	h := p.host
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.validateUseOwnerLocked(owner, maps.Values(p.instances))
}

// Preparation hands off its original instances. Looking them up again by unit
// would allow a successor with the same binding to replace that preparation.
func (p *bindingPreparation) captureLocked() (*capturedPlan, error) {
	for _, bound := range p.binding.Units {
		if err := p.instances[bound.Source].validateServingLocked(); err != nil {
			return nil, err
		}
	}
	plan := &capturedPlan{planHeader: planHeader{owner: p.host.domain, realm: p.realm, binding: p.binding.Clone()}}
	for _, source := range p.binding.Roots {
		instance := p.instances[source.UnitRef]
		export, err := instance.exports.bound(source)
		if err != nil {
			return nil, err
		}
		plan.includeMember(export.snapshotIn(p.realm))
	}
	for _, source := range p.binding.UnitRoots {
		instance := p.instances[source]
		plan.includeRoot(instance.id, source)
	}
	return plan, nil
}

func (p *bindingPreparation) abandon(cause error) {
	h := p.host
	h.mu.Lock()
	for _, instance := range p.created {
		instance.preparation.complete(cause)
		instance.beginRetireLocked()
	}
	h.mu.Unlock()
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
	snapshot, identity, err := binding.setupOf(bound.Source, unit.Declaration)
	if err != nil {
		return nil, false, err
	}
	key := instanceKey{source: bound.Source, realm: realm}
	if current := h.instances[key]; current != nil {
		if err := current.validateReuseLocked(identity, planned, grant); err != nil {
			return nil, false, err
		}
		return current, false, nil
	}
	if h.overlappingInstanceLocked(key) {
		return nil, false, fmt.Errorf("%w: original unit has not proved resource release", ErrInUse)
	}
	h.nextGeneration++
	lifetime := h.domain.NewLifetime(context.WithoutCancel(h.lifetime.Context()))
	preparation, err := newUnitPreparation(lifetime, bound.Source, reads)
	if err != nil {
		lifetime.BeginClose(err)
		return nil, false, err
	}
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
		preparation:   preparation,
		closeReady:    make(chan struct{}),
		retired:       make(chan struct{}),
		closeCauses:   lifetime.ObserveCloseCauses(),
	}
	instance.exports = newUnitExports(&h.catalog, instance, h.limits.MaxPublicationOperations)
	providers, err := instance.holdProvidersLocked(planned)
	if err == nil {
		err = h.registerInstanceLocked(instance, providers)
	}
	if err != nil {
		instance.preparation.complete(err)
		instance.releaseProvidersLocked()
		instance.lifetime.BeginClose(err)
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

// exportsOf names every declared export of this exact unit release.
func (d UnitDeclaration) exportsOf(source UnitRef) []ExportRef {
	exports := make([]ExportRef, 0, len(d.Exports))
	for _, export := range d.Exports {
		exports = append(exports, source.Export(export.Name, export.Contract))
	}
	return exports
}

// setupOf projects the exact graph one unit's setup depends on. The
// consumer's selection rule belongs to its capture, never to setup identity.
func (b BindingSnapshot) setupOf(unit UnitRef, declaration UnitDeclaration) (BindingSnapshot, framevalue.Digest, error) {
	binding, err := b.reachable(declaration.exportsOf(unit), []UnitRef{unit})
	if err != nil {
		return BindingSnapshot{}, framevalue.Digest{}, err
	}
	binding.Selection = framevalue.Digest{}
	identity, err := binding.Identity()
	return binding, identity, err
}
