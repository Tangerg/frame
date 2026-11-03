package frame

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"

	framevalue "github.com/Tangerg/frame/value"
)

// activationBinding yields the exact graph an activation realizes, in provider
// order. It runs under the activation's admission and locks Host.mu itself.
type activationBinding func(admitted context.Context) (BindingSnapshot, []bindingRead, error)

// Canceling the caller stops its wait; accepted attempts belong to the Host.
func (h *Host) Activate(ctx context.Context, roots ...ExportRef) (BindingSnapshot, error) {
	return h.ActivateIn(ctx, "", roots...)
}

func (h *Host) ActivateIn(ctx context.Context, realm RealmID, roots ...ExportRef) (BindingSnapshot, error) {
	return h.activate(ctx, realm, ScopeGrant{}, h.resolvedBinding(realm, roots))
}

// ActivatePlan admits a declaration plan and rechecks its original candidate
// reads through preparation. Recovery reads an accepted binding from its business owner.
func (h *Host) ActivatePlan(ctx context.Context, realm RealmID, plan *BindingPlan) (BindingSnapshot, error) {
	if plan == nil || plan.preparation == nil || plan.realm != realm {
		return BindingSnapshot{}, ErrInvalid
	}
	return h.activate(ctx, realm, ScopeGrant{}, h.plannedBinding(plan))
}

func (h *Host) ActivateUnit(ctx context.Context, realm RealmID, root UnitRef) (BindingSnapshot, error) {
	plan, err := h.PlanUnit(ctx, realm, root, nil)
	if err != nil {
		return BindingSnapshot{}, err
	}
	return h.ActivatePlan(ctx, realm, plan)
}

func (h *Host) resolvedBinding(realm RealmID, roots []ExportRef) activationBinding {
	return func(context.Context) (BindingSnapshot, []bindingRead, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		plan, err := h.resolveLocked(roots, nil, realm, nil)
		if err != nil {
			return BindingSnapshot{}, nil, err
		}
		return plan.binding, plan.preparation.reads, nil
	}
}

func (h *Host) plannedBinding(plan *BindingPlan) activationBinding {
	return func(context.Context) (BindingSnapshot, []bindingRead, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := h.validatePlanLocked(plan); err != nil {
			return BindingSnapshot{}, nil, err
		}
		return plan.binding.Clone(), plan.preparation.reads, nil
	}
}

// acceptedBinding restores the graph its business owner committed. It never
// resolves providers again; it only checks that graph against the installations.
func (h *Host) acceptedBinding(recordID string, grant ScopeGrant) activationBinding {
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

func (h *Host) activate(ctx context.Context, realm RealmID, grant ScopeGrant, bind activationBinding) (BindingSnapshot, error) {
	var binding BindingSnapshot
	var selected, created []*Instance
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
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.lifetime.isClosing() {
			return ErrClosed
		}
		planned := make(map[UnitRef]*Instance, len(binding.Units))
		for _, bound := range binding.Units {
			instance, isNew, err := h.planInstanceLocked(binding, bound, realm, reads, planned, grant)
			if err != nil {
				return err
			}
			planned[bound.Source] = instance
			selected = append(selected, instance)
			if isNew {
				created = append(created, instance)
			}
		}
		return nil
	})
	if err != nil {
		h.abandon(created, err)
		return BindingSnapshot{}, err
	}
	for _, instance := range created {
		go instance.prepare()
	}
	return binding.Clone(), h.awaitReady(ctx, selected)
}

// abandon retires instances whose activation was refused before they prepared.
func (h *Host) abandon(created []*Instance, cause error) {
	h.mu.Lock()
	for _, instance := range created {
		instance.preparationErr = cause
		close(instance.prepared)
		h.retireInstanceLocked(instance)
	}
	h.mu.Unlock()
	for _, instance := range created {
		instance.beginRetire()
	}
}

// planInstanceLocked returns the live instance that already realizes bound
// under its exact setup identity, or creates one holding its planned providers.
func (h *Host) planInstanceLocked(binding BindingSnapshot, bound UnitBinding, realm RealmID, reads []bindingRead, planned map[UnitRef]*Instance, grant ScopeGrant) (*Instance, bool, error) {
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
	snapshot, identity, err := setupBinding(binding, unitExports(bound.Source, unit.Declaration), bound.Source)
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
	if h.retiringLocked(key) {
		return nil, false, fmt.Errorf("%w: original unit is still retiring", ErrInUse)
	}
	h.nextGeneration++
	instance := &Instance{
		host:                  h,
		id:                    rand.Text(),
		generation:            h.nextGeneration,
		scope:                 installation.scope,
		startGrant:            grant,
		realm:                 realm,
		unit:                  unit.Declaration.Clone(),
		binding:               bound.Clone(),
		snapshot:              snapshot,
		identity:              identity,
		configuration:         bytes.Clone(installation.record.Configuration),
		setup:                 unit.Setup,
		lifetime:              NewLifetime(context.WithoutCancel(h.lifetime.Context())),
		setupExports:          make(map[string]*publication),
		publicationOperations: make(map[string]*publicationOperation),
		prepared:              make(chan struct{}),
		closed:                make(chan struct{}),
	}
	for _, read := range reads {
		if read.unit == bound.Source {
			instance.preparation = append(instance.preparation, read)
		}
	}
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

// retiringLocked reports an earlier instance of the same unit and realm whose
// retirement is unconfirmed; a successor must not overlap its resources.
func (h *Host) retiringLocked(key instanceKey) bool {
	for _, instance := range h.allInstances {
		source := instance.binding.Source
		if source.InstallationID == key.source.InstallationID && source.UnitID == key.source.UnitID && instance.realm == key.realm {
			return true
		}
	}
	return false
}

func unitExports(source UnitRef, unit UnitDeclaration) []ExportRef {
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

// registerInstanceLocked makes instance a Host resource that retires after its
// consumers, and retires it when a provider, its start grant or itself ends.
func (h *Host) registerInstanceLocked(instance *Instance, providers []*ResourceHandle) error {
	causes := instance.lifetime.ObserveCloseCauses()
	resource, err := h.lifetime.Own(Resource{
		Name:   instanceResourceName(instance.id),
		Cancel: instance.lifetime.cancel,
		Close: func(ctx context.Context) (ReleaseState, error) {
			instance.beginRetire()
			outcome, err := instance.retire(ctx)
			return releaseState(outcome, causes.join(err))
		},
	}, providers...)
	if err != nil {
		return err
	}
	instance.resource = resource
	h.instances[instance.key()] = instance
	h.allInstances[instance.id] = instance
	retireAfter := func(ctx context.Context, cause func() error) {
		instance.stops = append(instance.stops, context.AfterFunc(ctx, func() {
			instance.lifetime.cancel(cause())
			instance.beginRetire()
		}))
	}
	for _, hold := range instance.providerHolds {
		retireAfter(hold.Context(), func() error { return context.Cause(hold.Context()) })
	}
	if instance.startGrant.issued() {
		retireAfter(instance.startGrant.context, func() error { return ErrFenced })
	}
	instance.stops = append(instance.stops, context.AfterFunc(instance.lifetime.Context(), instance.beginRetire))
	return nil
}

func (h *Host) awaitReady(ctx context.Context, selected []*Instance) error {
	for _, instance := range selected {
		select {
		case <-instance.prepared:
		case <-ctx.Done():
			return contextError(ctx)
		}
		h.mu.Lock()
		err, retiring := instance.preparationErr, instance.retiring
		h.mu.Unlock()
		if err != nil {
			return err
		}
		if retiring {
			return ErrStaleBinding
		}
	}
	return nil
}

// Retire withdraws every current instance of an installation. Instances keep
// their resources until their consumers have drained.
func (h *Host) Retire(ctx context.Context, id InstallationID) error {
	var retired []*Instance
	err := h.admission.Admit(ctx, func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		for key, instance := range h.instances {
			if key.source.InstallationID == id {
				retired = append(retired, instance)
			}
		}
		for _, instance := range retired {
			h.retireInstanceLocked(instance)
		}
		return nil
	})
	for _, instance := range retired {
		instance.beginRetire()
	}
	return err
}

func (h *Host) RetireInstance(ctx context.Context, id string) error {
	var instance *Instance
	err := h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		instance = h.allInstances[id]
		if instance == nil {
			return ErrUnavailable
		}
		h.retireInstanceLocked(instance)
		return nil
	})
	if err != nil {
		return err
	}
	instance.beginRetire()
	return nil
}

// WaitInstanceRetired joins exactly the original instance and does not wait for
// other Units or later generations of the same installation.
func (h *Host) WaitInstanceRetired(ctx context.Context, id string) error {
	h.mu.Lock()
	instance := h.allInstances[id]
	h.mu.Unlock()
	if instance == nil {
		return nil
	}
	return awaitClosed(ctx, instance)
}

func (h *Host) WaitRetired(ctx context.Context, id InstallationID) error {
	h.mu.Lock()
	var instances []*Instance
	for _, instance := range h.allInstances {
		if instance.binding.Source.InstallationID == id {
			instances = append(instances, instance)
		}
	}
	h.mu.Unlock()
	for _, instance := range instances {
		if err := awaitClosed(ctx, instance); err != nil {
			return err
		}
	}
	return nil
}

func awaitClosed(ctx context.Context, instance *Instance) error {
	select {
	case <-instance.closed:
		return nil
	case <-ctx.Done():
		return contextError(ctx)
	}
}
