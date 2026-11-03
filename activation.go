package frame

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
)

type instanceKey struct {
	source UnitRef
	realm  RealmID
}

func (i *Instance) key() instanceKey { return instanceKey{source: i.binding.Source, realm: i.realm} }

func (h *Host) realmLocked(unit UnitDeclaration, realm RealmID) (RealmID, error) {
	if h.drivers[unit.Profile].realmBound {
		if err := realm.Validate(); err != nil {
			return "", err
		}
		return realm, nil
	}
	return "", nil
}

// Canceling the caller stops its wait; accepted attempts belong to the Host.
func (h *Host) Activate(ctx context.Context, roots ...ExportRef) (BindingSnapshot, error) {
	return h.activate(ctx, "", "", nil, roots, ScopeGrant{})
}
func (h *Host) ActivateIn(ctx context.Context, realm RealmID, roots ...ExportRef) (BindingSnapshot, error) {
	return h.activate(ctx, realm, "", nil, roots, ScopeGrant{})
}

// ActivatePlan admits a declaration plan and rechecks its original candidate
// reads through preparation. Recovery reads an accepted binding from its business owner.
func (h *Host) ActivatePlan(ctx context.Context, realm RealmID, plan *BindingPlan) (BindingSnapshot, error) {
	if plan == nil || plan.preparation == nil || plan.realm != realm {
		return BindingSnapshot{}, ErrInvalid
	}
	return h.activate(ctx, realm, "", plan, plan.binding.Roots, ScopeGrant{})
}

func (h *Host) ActivateUnit(ctx context.Context, realm RealmID, root UnitRef) (BindingSnapshot, error) {
	plan, err := h.PlanUnit(ctx, realm, root, nil)
	if err != nil {
		return BindingSnapshot{}, err
	}
	return h.ActivatePlan(ctx, realm, plan)
}

func (h *Host) activate(ctx context.Context, realm RealmID, recordID string, plan *BindingPlan, roots []ExportRef, grant ScopeGrant) (BindingSnapshot, error) {
	var binding BindingSnapshot
	var selected, created []*Instance
	err := h.admission.Admit(ctx, func(admitted context.Context) error {
		if grant.owner != nil {
			if err := h.admission.ValidateScope(grant); err != nil {
				return err
			}
		}
		var frozen *BindingSnapshot
		if recordID != "" {
			original, err := h.readAcceptedBinding(admitted, recordID, grant)
			if err != nil {
				return err
			}
			snapshot := original.Clone()
			frozen = &snapshot
			roots = snapshot.Roots
			if err := h.ValidateBindingUnder(admitted, *frozen); err != nil {
				return err
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed {
			return ErrClosed
		}
		var reads []bindingRead
		var err error
		binding, reads, err = h.activationBindingLocked(realm, frozen, plan, roots)
		if err != nil {
			return err
		}
		planned := make(map[UnitRef]*Instance, len(binding.Units))
		for _, bound := range binding.Units {
			instance, isNew, err := h.planInstanceLocked(binding, bound, realm, reads, planned, grant)
			if err != nil {
				return err
			}
			planned[bound.Source] = instance
			if isNew {
				created = append(created, instance)
			}
			selected = append(selected, instance)
		}
		return nil
	})
	if err != nil {
		for _, instance := range created {
			h.mu.Lock()
			instance.readyErr = err
			instance.readyDone = true
			close(instance.ready)
			instance.retiring = true
			delete(h.instances, instance.key())
			h.mu.Unlock()
			instance.beginRetire()
		}
		return BindingSnapshot{}, err
	}
	for _, instance := range created {
		go instance.prepare()
	}
	return binding.Clone(), h.awaitReady(ctx, selected)
}

// activationBindingLocked chooses what to activate: a frozen binding's subset,
// an already resolved plan, or a fresh resolution of roots.
func (h *Host) activationBindingLocked(realm RealmID, frozen *BindingSnapshot, plan *BindingPlan, roots []ExportRef) (BindingSnapshot, []bindingRead, error) {
	switch {
	case frozen != nil:
		binding, err := subsetBinding(*frozen, roots)
		return binding, nil, err
	case plan != nil:
		if err := h.validatePlanLocked(plan); err != nil {
			return BindingSnapshot{}, nil, err
		}
		return plan.binding.Clone(), plan.preparation.reads, nil
	default:
		resolved, err := h.resolveLocked(roots, nil, realm, nil)
		if err != nil {
			return BindingSnapshot{}, nil, err
		}
		return resolved.binding.Clone(), resolved.preparation.reads, nil
	}
}

// planInstanceLocked returns the live instance that already realizes bound
// under its exact setup identity, or creates one holding its planned providers.
func (h *Host) planInstanceLocked(binding BindingSnapshot, bound UnitBinding, realm RealmID, reads []bindingRead, planned map[UnitRef]*Instance, grant ScopeGrant) (*Instance, bool, error) {
	unit, installation, err := h.unitLocked(bound.Source)
	if err != nil {
		return nil, false, err
	}
	actualRealm, err := h.realmLocked(unit.Declaration, realm)
	if err != nil {
		return nil, false, err
	}
	key := instanceKey{source: bound.Source, realm: actualRealm}
	if err := h.admission.ValidateScope(installation.scope); err != nil {
		return nil, false, err
	}
	var unitRoots []ExportRef
	for _, export := range unit.Declaration.Exports {
		unitRoots = append(unitRoots, ExportRef{InstallationID: bound.Source.InstallationID, Release: bound.Source.Release, UnitID: bound.Source.UnitID, Export: export.Name, Contract: export.Contract})
	}
	actual, err := subsetBindingRoots(binding, unitRoots, []UnitRef{bound.Source})
	if err != nil {
		return nil, false, err
	}
	// The consumer's ordering rule belongs to its capture. It cannot
	// change an already approved Unit's exact setup identity.
	actual.Rule = ""
	identity, err := actual.Identity()
	if err != nil {
		return nil, false, err
	}
	if current := h.instances[key]; current != nil && !current.retiring {
		if current.startGrant.owner != nil && current.startGrant != grant {
			return nil, false, ErrFenced
		}
		if current.identity != identity {
			return nil, false, fmt.Errorf("%w: unit already has another setup binding", ErrInUse)
		}
		return current, false, nil
	}
	for _, old := range h.allInstances {
		if old.binding.Source.InstallationID == bound.Source.InstallationID && old.binding.Source.UnitID == bound.Source.UnitID && old.realm == actualRealm {
			return nil, false, fmt.Errorf("%w: original unit is still retiring", ErrInUse)
		}
	}
	id, err := randomID()
	if err != nil {
		return nil, false, err
	}
	h.nextGeneration++
	instance := &Instance{host: h, startGrant: grant, id: id, generation: h.nextGeneration, scope: installation.scope, realm: actualRealm, unit: unit.Declaration.Clone(), binding: bound.Clone(), snapshot: actual, identity: identity, configuration: bytes.Clone(installation.record.Configuration), setup: unit.Setup, lifetime: NewLifetime(context.WithoutCancel(h.lifetime.Context())), setupExports: make(map[string]*publication), ready: make(chan struct{}), closed: make(chan struct{}), changed: make(chan struct{}), publicationOperations: make(map[string]*publicationOperation)}
	for _, read := range reads {
		if read.unit == bound.Source {
			instance.preparation = append(instance.preparation, read)
		}
	}
	resourceDependencies, err := instance.holdProvidersLocked(planned)
	if err != nil {
		instance.releaseDependencyHoldsLocked()
		return nil, false, err
	}
	if err := h.registerInstanceLocked(instance, key, resourceDependencies); err != nil {
		instance.releaseDependencyHoldsLocked()
		return nil, false, err
	}
	return instance, true, nil
}

// holdProvidersLocked enters every provider instance the binding planned
// before i, so a provider cannot retire beneath its consumer.
func (i *Instance) holdProvidersLocked(planned map[UnitRef]*Instance) ([]*ResourceHandle, error) {
	var resourceDependencies []*ResourceHandle
	seen := make(map[*Instance]bool)
	for _, requirement := range i.binding.Imports {
		for _, provider := range requirement.Providers {
			if provider.Unit() == i.binding.Source {
				continue
			}
			dependency := planned[provider.Unit()]
			if dependency == nil {
				return nil, fmt.Errorf("%w: provider not ordered before consumer", ErrInvalid)
			}
			if seen[dependency] {
				continue
			}
			seen[dependency] = true
			hold, err := dependency.lifetime.Enter(context.WithoutCancel(i.host.lifetime.Context()))
			if err != nil {
				return nil, err
			}
			i.dependencies = append(i.dependencies, dependency)
			i.dependencyHolds = append(i.dependencyHolds, hold)
			dependency.dependents++
			dependency.signalLocked()
			resourceDependencies = append(resourceDependencies, dependency.hostResource)
		}
	}
	return resourceDependencies, nil
}

// registerInstanceLocked makes instance a host resource that retires after its
// dependents, and retires it when a held provider or its own lifetime ends.
func (h *Host) registerInstanceLocked(instance *Instance, key instanceKey, resourceDependencies []*ResourceHandle) error {
	resource, err := h.lifetime.Own(Resource{Name: "unit:" + instance.id, Cancel: func(cause error) { instance.lifetime.cancel(cause) }, Close: func(closeCtx context.Context) (ReleaseState, error) {
		instance.beginRetire()
		outcome, err := instance.retire(closeCtx)
		if outcome.Released {
			return ResourceReleased, err
		}
		return ResourceUnconfirmed, withoutUnconfirmedState(err)
	}}, resourceDependencies...)
	if err != nil {
		return err
	}
	instance.hostResource = resource
	h.instances[key] = instance
	h.allInstances[instance.id] = instance
	for _, hold := range instance.dependencyHolds {
		instance.stopDependencies = append(instance.stopDependencies, context.AfterFunc(hold.Context(), func() {
			instance.lifetime.cancel(context.Cause(hold.Context()))
			instance.beginRetire()
		}))
	}
	if instance.startGrant.owner != nil {
		instance.stopDependencies = append(instance.stopDependencies, context.AfterFunc(instance.startGrant.context, func() { instance.lifetime.cancel(ErrFenced); instance.beginRetire() }))
	}
	instance.stopFailure = context.AfterFunc(instance.lifetime.Context(), func() { instance.beginRetire() })
	return nil
}

func (h *Host) awaitReady(ctx context.Context, selected []*Instance) error {
	for _, instance := range selected {
		select {
		case <-instance.ready:
		case <-ctx.Done():
			return contextError(ctx)
		}
		h.mu.Lock()
		err := instance.readyErr
		retiring := instance.retiring
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

func (i *Instance) prepare() {
	h := i.host
	work, err := i.lifetime.Enter(i.lifetime.Context())
	if err != nil {
		i.finishPrepare(nil, nil, err)
		return
	}
	defer work.Finish()
	setup := &Setup{host: h, instance: i, work: work}
	for _, dependency := range i.dependencies {
		select {
		case <-dependency.ready:
		case <-work.Context().Done():
			i.finishPrepare(nil, nil, contextError(work.Context()))
			return
		}
		h.mu.Lock()
		err := dependency.readyErr
		retiring := dependency.retiring
		h.mu.Unlock()
		if err != nil || retiring {
			i.finishPrepare(nil, nil, errors.Join(ErrUnavailable, err))
			return
		}
	}
	h.mu.Lock()
	setup.imports, err = i.bindImportsLocked()
	driver := h.drivers[i.unit.Profile].Driver
	h.mu.Unlock()
	if err != nil {
		i.finishPrepare(nil, nil, err)
		return
	}
	var exports []ExportValue
	if i.unit.Profile == NativeGo {
		exports, err = invokeSetup(i.setup, work.Context(), setup)
	} else if driver == nil {
		err = ErrUnavailable
	} else {
		exports, err = invokeSetup(driver.Prepare, work.Context(), setup)
	}
	if err == nil {
		err = contextError(work.Context())
	}
	i.finishPrepare(exports, setup.sealPreparation(), err)
}

func (i *Instance) bindImportsLocked() (map[string][]BoundValue, error) {
	imports := make(map[string][]BoundValue, len(i.binding.Imports))
	for _, requirement := range i.binding.Imports {
		values := make([]BoundValue, 0, len(requirement.Providers))
		for _, provider := range requirement.Providers {
			if provider.Unit() == i.binding.Source {
				values = append(values, BoundValue{Source: provider, Local: true})
				continue
			}
			selected, err := i.providerExportLocked(provider)
			if err != nil {
				return nil, err
			}
			values = append(values, BoundValue{Source: provider, Value: selected.value})
		}
		imports[requirement.Slot] = values
	}
	return imports, nil
}

func (i *Instance) providerExportLocked(source ExportRef) (*publication, error) {
	for _, dependency := range i.dependencies {
		if dependency.binding.Source == source.Unit() {
			return dependency.setupExportLocked(source)
		}
	}
	return nil, ErrContractMismatch
}

// A declaration names the original setup export. Live Point membership can
// withdraw that value, but cannot replace what a new setup would receive.
func (i *Instance) setupExportLocked(source ExportRef) (*publication, error) {
	selected := i.setupExports[source.Export]
	if selected == nil || selected.source != source {
		return nil, ErrContractMismatch
	}
	if !selected.visible || !selected.durable {
		return nil, ErrUnavailable
	}
	return selected, nil
}

func (i *Instance) validateImportsLocked() error {
	for _, requirement := range i.binding.Imports {
		for _, provider := range requirement.Providers {
			if provider.Unit() == i.binding.Source {
				continue
			}
			if _, err := i.providerExportLocked(provider); err != nil {
				return err
			}
		}
	}
	return nil
}

func invokeSetup(setup SetupFunc, ctx context.Context, value *Setup) (exports []ExportValue, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("frame: setup panicked: %v", recovered)
		}
	}()
	return setup(ctx, value)
}

func (i *Instance) validateScopes() error {
	if i.startGrant.owner != nil {
		if err := i.host.admission.ValidateScope(i.startGrant); err != nil {
			return err
		}
	}
	return i.host.admission.ValidateScope(i.scope)
}

func (i *Instance) validateCurrentLocked() error {
	h := i.host
	if h.closed || i.retiring || h.instances[i.key()] != i || i.lifetime.Context().Err() != nil {
		return ErrStaleBinding
	}
	if err := i.validateScopes(); err != nil {
		return err
	}
	if err := h.validateBindingLocked(i.snapshot); err != nil {
		return err
	}
	if !i.readyDone {
		if err := i.validateImportsLocked(); err != nil {
			return err
		}
	}
	return h.validateBindingReadsLocked(i.preparation)
}

func (i *Instance) finishPrepare(exports []ExportValue, activate func(context.Context) error, setupErr error) {
	h := i.host
	var prepared []*publication
	if setupErr == nil {
		prepared, setupErr = h.prepareExports(i, exports)
	}
	if setupErr == nil && activate != nil {
		setupErr = h.admission.Admit(i.lifetime.Context(), func(context.Context) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return i.validateCurrentLocked()
		})
		if setupErr == nil {
			setupErr = invokeActivation(activate, i.lifetime.Context())
		}
	}
	if setupErr == nil {
		setupErr = h.admission.Admit(i.lifetime.Context(), func(context.Context) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if err := i.validateCurrentLocked(); err != nil {
				return err
			}
			for _, dependency := range i.dependencies {
				if dependency.retiring || dependency.readyErr != nil {
					return ErrStaleBinding
				}
			}
			if h.publicationCountLocked()+len(prepared) > h.maxPublications {
				return ErrCapacity
			}
			for _, publication := range prepared {
				h.publishLocked(publication)
				i.setupExports[publication.source.Export] = publication
			}
			i.preparation = nil
			return nil
		})
	}
	h.mu.Lock()
	i.readyErr = setupErr
	i.readyDone = true
	i.preparation = nil
	close(i.ready)
	if setupErr != nil {
		i.retiring = true
		if h.instances[i.key()] == i {
			delete(h.instances, i.key())
		}
	}
	h.mu.Unlock()
	if setupErr != nil {
		i.beginRetire()
	}
}

func (i *Instance) signalLocked() { close(i.changed); i.changed = make(chan struct{}) }

func (i *Instance) retire(ctx context.Context) (CloseOutcome, error) {
	h := i.host
	h.mu.Lock()
	i.retiring = true
	h.withdrawInstanceLocked(i)
	preparing := !i.readyDone
	h.mu.Unlock()
	if preparing {
		i.lifetime.cancel(ErrClosed)
	}
	select {
	case <-i.ready:
	case <-ctx.Done():
		return CloseOutcome{}, contextError(ctx)
	}
	for {
		h.mu.Lock()
		uses := i.uses + i.dependents
		changed := i.changed
		h.mu.Unlock()
		if uses == 0 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return CloseOutcome{}, contextError(ctx)
		}
	}
	return i.lifetime.BeginClose(ErrClosed).Wait(ctx)
}

func (i *Instance) beginRetire() {
	i.retireOnce.Do(func() {
		go func() {
			_, _ = i.retire(context.Background())
			outcome, _ := i.lifetime.BeginClose(ErrClosed).WaitReleased(context.Background())
			if !outcome.Released {
				return
			}
			if i.stopFailure != nil {
				i.stopFailure()
			}
			for _, stop := range i.stopDependencies {
				stop()
			}
			for _, hold := range i.dependencyHolds {
				hold.Finish()
			}
			_ = i.hostResource.ConfirmReleased(outcomeError(outcome))
			h := i.host
			h.mu.Lock()
			for _, dependency := range i.dependencies {
				dependency.dependents--
				dependency.signalLocked()
			}
			if h.instances[i.key()] == i {
				delete(h.instances, i.key())
			}
			delete(h.allInstances, i.id)
			i.setupExports = nil
			i.dependencies = nil
			i.dependencyHolds = nil
			i.setup = nil
			i.configuration = nil
			i.publicationOperations = nil
			i.stopFailure = nil
			i.stopDependencies = nil
			close(i.closed)
			h.mu.Unlock()
		}()
	})
}

func (h *Host) Retire(ctx context.Context, id InstallationID) error {
	var values []*Instance
	err := h.admission.Admit(ctx, func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		for ref, instance := range h.instances {
			if ref.source.InstallationID == id {
				instance.retiring = true
				h.withdrawInstanceLocked(instance)
				delete(h.instances, ref)
				values = append(values, instance)
			}
		}
		return nil
	})
	for _, instance := range values {
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
		instance.retiring = true
		h.withdrawInstanceLocked(instance)
		if h.instances[instance.key()] == instance {
			delete(h.instances, instance.key())
		}
		return nil
	})
	if err == nil {
		instance.beginRetire()
	}
	return err
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
	select {
	case <-instance.closed:
		return nil
	case <-ctx.Done():
		return contextError(ctx)
	}
}

func invokeActivation(activate func(context.Context) error, ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("frame: activation panicked: %v", recovered)
		}
	}()
	return activate(ctx)
}

func (h *Host) WaitRetired(ctx context.Context, id InstallationID) error {
	h.mu.Lock()
	var values []*Instance
	for _, instance := range h.allInstances {
		if instance.binding.Source.InstallationID == id {
			values = append(values, instance)
		}
	}
	h.mu.Unlock()
	for _, instance := range values {
		select {
		case <-instance.closed:
		case <-ctx.Done():
			return contextError(ctx)
		}
	}
	return nil
}

func (h *Host) ActiveSources() []ExportRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	var values []ExportRef
	for _, store := range h.catalog {
		for _, publication := range store.members {
			values = append(values, publication.source)
		}
	}
	slices.SortFunc(values, func(a, b ExportRef) int {
		if value := compareUnit(a.Unit(), b.Unit()); value != 0 {
			return value
		}
		if a.Export < b.Export {
			return -1
		}
		if a.Export > b.Export {
			return 1
		}
		return 0
	})
	return values
}

// Host.mu is held. A preparation that never acquired its Host resource cannot
// leave a provider pinned by a partial graph allocation.
func (i *Instance) releaseDependencyHoldsLocked() {
	for _, stop := range i.stopDependencies {
		stop()
	}
	for index, hold := range i.dependencyHolds {
		hold.Finish()
		provider := i.dependencies[index]
		provider.dependents--
		provider.signalLocked()
	}
	i.stopDependencies = nil
	i.dependencyHolds = nil
	i.dependencies = nil
}
