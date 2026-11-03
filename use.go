package frame

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

type UseConfig struct {
	Owner       *Lifetime
	Scope       ScopeGrant
	Parent      *Use
	Realm       RealmID
	ExecutionID string
}

type Use struct {
	host               *Host
	owner              *authorityIdentity
	settlementMu       sync.Mutex
	id                 string
	parentID           string
	executionID        string
	scope              ScopeGrant
	realm              RealmID
	lifetime           *Lifetime
	binding            BindingSnapshot
	members            map[string]*publication
	selections         map[string][]string
	instances          []*Instance
	instanceHolds      []*Work
	stopDependencies   []func() bool
	hostResource       *ResourceHandle
	ownerResource      *ResourceHandle
	retireOnce         sync.Once
	closed             chan struct{}
	releaseErr         error
	initialized        chan struct{}
	protected          bool
	settlementChanged  chan struct{}
	settlementRevision uint64
	retrySettlement    chan struct{}
	settled            bool
}

func (u *Use) ID() string               { return u.id }
func (u *Use) Context() context.Context { return u.lifetime.Context() }
func (u *Use) Lifetime() *Lifetime      { return u.lifetime }
func (u *Use) Binding() BindingSnapshot { return u.binding.Clone() }
func (u *Use) Realm() RealmID           { return u.realm }

func (u *Use) enter(ctx context.Context) (*Work, error) {
	work, err := u.lifetime.Enter(ctx)
	if err != nil {
		return nil, err
	}
	if err := u.host.admission.ValidateScope(u.scope); err != nil {
		work.Finish()
		return nil, err
	}
	return work, nil
}

// ValidateUse checks the original accepted permission. Withdrawal of a Point
// member does not rewrite this Use or require durable reconstruction identity.
func (h *Host) ValidateUse(ctx context.Context, use *Use) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if use == nil || use.owner != h.identity {
		return ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() || h.uses[use.id] != use {
		return ErrClosed
	}
	if err := h.admission.ValidateScope(use.scope); err != nil {
		return err
	}
	use.lifetime.mu.Lock()
	closing := use.lifetime.closing != nil || use.lifetime.released
	use.lifetime.mu.Unlock()
	if closing {
		return ErrClosed
	}
	return nil
}

func (h *Host) Acquire(ctx context.Context, plan *BindingPlan, config UseConfig) (*Use, error) {
	if ctx == nil || plan == nil || config.Owner == nil {
		return nil, ErrInvalid
	}
	config, err := h.inheritParent(config)
	if err != nil {
		return nil, err
	}
	if config.Realm != "" && h.protection == nil {
		return nil, fmt.Errorf("%w: surviving execution requires protection", ErrInvalid)
	}
	if config.Realm != plan.realm {
		return nil, ErrContractMismatch
	}
	if plan.preparation != nil {
		preparing, err := config.Owner.Enter(ctx)
		if err != nil {
			return nil, err
		}
		defer preparing.Finish()
		if err := h.admission.ValidateScope(config.Scope); err != nil {
			return nil, err
		}
		binding, err := h.activate(preparing.Context(), config.Realm, "", plan, plan.binding.Roots, config.Scope)
		if err != nil {
			return nil, err
		}
		return h.acquireReadyBinding(preparing.Context(), binding, config, "")
	}
	return h.acquirePrepared(ctx, plan, config, "")
}

func (h *Host) acquirePrepared(ctx context.Context, plan *BindingPlan, config UseConfig, recordID string) (*Use, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	use := &Use{host: h, owner: h.identity, id: id, executionID: config.ExecutionID, scope: config.Scope, realm: config.Realm, lifetime: NewLifetime(context.WithoutCancel(ctx)), binding: plan.binding.Clone(), members: make(map[string]*publication), selections: make(map[string][]string), closed: make(chan struct{}), initialized: make(chan struct{}), settlementChanged: make(chan struct{}), retrySettlement: make(chan struct{}, 1)}
	if config.Parent != nil {
		use.parentID = config.Parent.id
	}
	err = h.admission.Admit(ctx, func(admitted context.Context) error {
		if err := h.admission.ValidateScope(config.Scope); err != nil {
			return err
		}
		if recordID != "" {
			authorized, err := h.readAcceptedBinding(admitted, recordID, config.Scope)
			if err != nil {
				return err
			}
			same, err := use.binding.sameIdentity(authorized)
			if err != nil {
				return err
			}
			if !same {
				return ErrStaleBinding
			}
		}
		if err := h.attachPlanMembers(ctx, use, plan, config.Owner); err != nil {
			return err
		}
		if err := h.protectUse(admitted, use, config); err != nil {
			return err
		}
		return h.publishUse(use, config.Owner)
	})
	close(use.initialized)
	if err != nil {
		use.BeginClose(err)
		return nil, err
	}
	return use, nil
}

// A child must remain inside its parent's execution, realm and scope; another
// owner would detach protected work from the permission that admitted it.
func (h *Host) inheritParent(config UseConfig) (UseConfig, error) {
	parent := config.Parent
	if parent == nil {
		return config, nil
	}
	if parent.owner != h.identity || config.Owner != parent.lifetime {
		return config, ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() || h.uses[parent.id] != parent {
		return config, ErrClosed
	}
	if config.Scope.owner == nil {
		config.Scope = parent.scope
	}
	if config.Realm == "" {
		config.Realm = parent.realm
	}
	if config.ExecutionID == "" {
		config.ExecutionID = parent.executionID
	}
	if config.ExecutionID != parent.executionID || config.Realm != parent.realm || config.Scope != parent.scope {
		return config, ErrInvalid
	}
	return config, nil
}

func (h *Host) attachPlanMembers(ctx context.Context, use *Use, plan *BindingPlan, owner *Lifetime) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.validatePlanLocked(plan); err != nil {
		return err
	}
	if len(h.uses) >= h.maxUses {
		return fmt.Errorf("%w: active uses", ErrCapacity)
	}
	seen := make(map[*Instance]bool)
	for _, member := range plan.members {
		actual := h.catalog[member.source.Contract].members[member.id]
		use.members[member.id] = actual
		if !seen[actual.instance] {
			seen[actual.instance] = true
			use.instances = append(use.instances, actual.instance)
		}
	}
	for _, member := range plan.unitMembers {
		instance := h.allInstances[member.instanceID]
		if !seen[instance] {
			seen[instance] = true
			use.instances = append(use.instances, instance)
		}
	}
	if err := h.validateUseOwnerLocked(owner, use.instances); err != nil {
		return err
	}
	for _, selection := range plan.selections {
		use.selections[selection.id] = []string{}
		for _, member := range selection.members {
			use.selections[selection.id] = append(use.selections[selection.id], member.id)
		}
	}
	for _, instance := range use.instances {
		// Activation may finish before its instance is retired and rebuilt.
		// Capture ordering belongs to the Use; the full setup graph must still
		// have the identity originally admitted for this particular instance.
		binding, err := subsetBindingRoots(plan.binding, instance.snapshot.Roots, instance.snapshot.UnitRoots)
		if err != nil {
			return errors.Join(ErrStaleBinding, err)
		}
		binding.Rule = instance.snapshot.Rule
		identity, err := binding.Identity()
		if err != nil {
			return errors.Join(ErrStaleBinding, err)
		}
		if identity != instance.identity {
			return fmt.Errorf("%w: unit acquired another setup binding", ErrStaleBinding)
		}
		if instance.startGrant.owner != nil && instance.startGrant != use.scope {
			return ErrFenced
		}
		hold, err := instance.lifetime.Enter(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}
		use.instanceHolds = append(use.instanceHolds, hold)
		stop := context.AfterFunc(hold.Context(), func() { use.BeginClose(context.Cause(hold.Context())) })
		use.stopDependencies = append(use.stopDependencies, stop)
	}
	return nil
}

// An instance drains its consumers before closing owned resources. Accepting a
// Use owned by that instance, even through another Use, would make both wait for
// the other. Follow the original resource handles and provider edges only.
func (h *Host) validateUseOwnerLocked(owner *Lifetime, instances []*Instance) error {
	owners := make(map[*Lifetime]bool)
	for owner != nil && !owners[owner] {
		owners[owner] = true
		var parent *Lifetime
		for _, use := range h.uses {
			if use.lifetime == owner && use.ownerResource != nil {
				parent = use.ownerResource.lifetime
				break
			}
		}
		owner = parent
	}
	seen := make(map[*Instance]bool)
	pending := slices.Clone(instances)
	for len(pending) > 0 {
		instance := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[instance] {
			continue
		}
		if owners[instance.lifetime] {
			return fmt.Errorf("%w: use owner protects instance %s", ErrDependencyCycle, instance.id)
		}
		seen[instance] = true
		pending = append(pending, instance.dependencies...)
	}
	return nil
}

// protectUse records a surviving execution's Use approval before it is
// published, so a restart can still account for it.
func (h *Host) protectUse(admitted context.Context, use *Use, config UseConfig) error {
	if config.Realm == "" {
		return nil
	}
	if h.protection == nil {
		return fmt.Errorf("%w: surviving execution requires protection", ErrInvalid)
	}
	instanceIDs := make([]string, 0, len(use.instances))
	for _, instance := range use.instances {
		instanceIDs = append(instanceIDs, instance.id)
	}
	slices.Sort(instanceIDs)
	if err := h.protection.ApproveUse(admitted, UseApproval{ID: use.id, ParentID: use.parentID, ExecutionID: config.ExecutionID, Instances: instanceIDs, Realm: config.Realm, Scope: config.Scope.WireID(), ScopeGeneration: config.Scope.Generation(), Epoch: h.epoch, Binding: use.binding.Clone()}); err != nil {
		return err
	}
	use.protected = true
	return nil
}

func (h *Host) publishUse(use *Use, owner *Lifetime) error {
	causes := use.lifetime.ObserveCloseCauses()
	resource := Resource{Name: "use:" + use.id, Cancel: func(cause error) { use.BeginClose(cause) }, Close: func(closeCtx context.Context) (ReleaseState, error) {
		outcome, err := use.Close(closeCtx)
		err = causes.join(err)
		if outcome.Released {
			return ResourceReleased, err
		}
		return ResourceUnconfirmed, withoutUnconfirmedState(err)
	}}
	ownerResource, err := owner.Own(resource)
	if err != nil {
		return err
	}
	use.ownerResource = ownerResource
	var dependencies []*ResourceHandle
	for _, instance := range use.instances {
		dependencies = append(dependencies, instance.hostResource)
	}
	hostResource, err := h.lifetime.Own(resource, dependencies...)
	if err != nil {
		return err
	}
	use.hostResource = hostResource
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, instance := range use.instances {
		instance.uses++
		instance.signalLocked()
	}
	h.uses[use.id] = use
	if !use.scope.key.installation {
		use.stopDependencies = append(use.stopDependencies, context.AfterFunc(use.scope.context, func() { use.BeginClose(ErrFenced) }))
	}
	return nil
}

func (u *Use) BeginClose(cause error) *Close {
	causes := u.lifetime.ObserveCloseCauses()
	operation := u.lifetime.BeginClose(cause)
	u.retireOnce.Do(func() {
		go func() {
			// Owner cancellation may race resource registration. No fields used by
			// retirement are read until the admission has published or rejected.
			<-u.initialized
			outcome, _ := operation.WaitReleased(context.Background())
			if !outcome.Released {
				return
			}
			h := u.host
			for u.protected {
				err := h.protection.ReleaseUse(context.Background(), u.id, outcome)
				if err == nil {
					break
				}
				h.admission.RequireRecovery(err)
				u.settlementMu.Lock()
				u.releaseErr = err
				u.settlementRevision++
				close(u.settlementChanged)
				u.settlementChanged = make(chan struct{})
				u.settlementMu.Unlock()
				// A later explicit Close resumes this same original settlement.
				<-u.retrySettlement
			}
			for _, stop := range u.stopDependencies {
				stop()
			}
			for _, hold := range u.instanceHolds {
				hold.Finish()
			}
			if u.ownerResource != nil {
				_ = u.ownerResource.ConfirmReleased(causes.join(outcomeError(outcome)))
			}
			if u.hostResource != nil {
				_ = u.hostResource.ConfirmReleased(causes.join(outcomeError(outcome)))
			}
			h.mu.Lock()
			if _, exists := h.uses[u.id]; exists {
				delete(h.uses, u.id)
				for _, instance := range u.instances {
					instance.uses--
					instance.signalLocked()
				}
			}
			u.members = nil
			u.selections = nil
			u.instances = nil
			u.instanceHolds = nil
			u.stopDependencies = nil
			u.hostResource = nil
			u.ownerResource = nil
			u.scope = ScopeGrant{}
			u.host = nil
			h.mu.Unlock()
			u.settlementMu.Lock()
			u.releaseErr = nil
			u.settled = true
			u.settlementRevision++
			close(u.closed)
			close(u.settlementChanged)
			u.settlementMu.Unlock()
		}()
	})
	return operation
}

func (u *Use) Close(ctx context.Context) (CloseOutcome, error) {
	causes := u.lifetime.ObserveCloseCauses()
	outcome, err := u.BeginClose(ErrClosed).Wait(ctx)
	err = causes.join(err)
	if !outcome.Released {
		return outcome, err
	}
	return u.waitRetirement(ctx, outcome, err)
}

// WaitRetired joins the original exit, including stable execution-protection
// settlement and removal of Host references. It never starts a second cleanup.
func (u *Use) WaitRetired(ctx context.Context) (CloseOutcome, error) {
	if ctx == nil || u == nil {
		return CloseOutcome{}, ErrInvalid
	}
	select {
	case <-u.closed:
		u.lifetime.mu.Lock()
		outcome := u.lifetime.outcomeLocked(u.lifetime.closing.id)
		u.lifetime.mu.Unlock()
		return outcome, outcomeError(outcome)
	case <-ctx.Done():
		return CloseOutcome{}, contextError(ctx)
	}
}

func (u *Use) waitRetirement(ctx context.Context, outcome CloseOutcome, prior error) (CloseOutcome, error) {
	u.settlementMu.Lock()
	revision := u.settlementRevision
	if u.releaseErr != nil && !u.settled {
		select {
		case u.retrySettlement <- struct{}{}:
		default:
		}
	}
	for {
		if u.settled {
			u.settlementMu.Unlock()
			return outcome, prior
		}
		if u.settlementRevision > revision && u.releaseErr != nil {
			releaseErr := u.releaseErr
			u.settlementMu.Unlock()
			outcome.Released = false
			outcome.Pending = append(outcome.Pending, "use settlement:"+u.id)
			return outcome, errors.Join(prior, releaseErr)
		}
		changed := u.settlementChanged
		u.settlementMu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			outcome.Released = false
			outcome.Pending = append(outcome.Pending, "use settlement:"+u.id)
			return outcome, errors.Join(prior, contextError(ctx))
		}
		u.settlementMu.Lock()
	}
}

func Borrow[M, V any](ctx context.Context, use *Use, selection Selection[M, V], call func(context.Context, []V) error) error {
	if call == nil {
		return ErrInvalid
	}
	return withAccess(ctx, use, func(a *Access) error {
		members, err := selection.Members(a)
		if err != nil {
			return err
		}
		values := make([]V, len(members))
		for i, m := range members {
			values[i] = m.Value
		}
		return call(a.Context(), values)
	})
}

func BorrowExport[V any](ctx context.Context, use *Use, source ExportRef, call func(context.Context, V) error) error {
	if use == nil || call == nil {
		return ErrInvalid
	}
	work, err := use.enter(ctx)
	if err != nil {
		return err
	}
	defer work.Finish()
	use.host.mu.Lock()
	var value V
	found := false
	for _, publication := range use.members {
		if publication.source == source {
			value, found = publication.value.(V)
			break
		}
	}
	use.host.mu.Unlock()
	if !found {
		return ErrContractMismatch
	}
	return call(work.Context(), value)
}

// BorrowInstance is a trusted driver's access to the exact instance approved
// for a root Unit. It registers the call in the original Use before exposing an
// instance identity, including Units which publish no public object.
func BorrowInstance(ctx context.Context, use *Use, source UnitRef, call func(context.Context, string) error) error {
	if use == nil || call == nil {
		return ErrInvalid
	}
	work, err := use.enter(ctx)
	if err != nil {
		return err
	}
	defer work.Finish()
	use.host.mu.Lock()
	var id string
	for _, instance := range use.instances {
		if instance.binding.Source == source {
			id = instance.id
			break
		}
	}
	use.host.mu.Unlock()
	if id == "" {
		return ErrContractMismatch
	}
	return call(work.Context(), id)
}

func WithPlan(ctx context.Context, host *Host, plan *BindingPlan, scope ScopeGrant, call func(*Use) error) (err error) {
	if ctx == nil || host == nil || plan == nil || call == nil {
		return ErrInvalid
	}
	owner := NewLifetime(context.WithoutCancel(ctx))
	use, err := host.Acquire(ctx, plan, UseConfig{Owner: owner, Scope: scope, Realm: plan.realm})
	if err != nil {
		_, closeErr := owner.Close(context.Background())
		return errors.Join(err, closeErr)
	}
	defer func() {
		_, closeErr := owner.Close(context.Background())
		err = errors.Join(err, closeErr)
	}()
	return call(use)
}

func (h *Host) AcquireChild(ctx context.Context, parent *Use, plan *BindingPlan) (*Use, error) {
	if parent == nil {
		return nil, ErrInvalid
	}
	return h.Acquire(ctx, plan, UseConfig{Owner: parent.lifetime, Parent: parent})
}

// AcquireRecord restores the original owner's committed graph without rerunning
// selection. A serialized plan snapshot alone cannot authorize this path.
func (h *Host) AcquireRecord(ctx context.Context, recordID string, config UseConfig) (*Use, error) {
	if ctx == nil || config.Owner == nil || h.readAcceptedBinding == nil || !ValidName(recordID) {
		return nil, ErrInvalid
	}
	config, err := h.inheritParent(config)
	if err != nil {
		return nil, err
	}
	if config.Realm != "" && h.protection == nil {
		return nil, fmt.Errorf("%w: surviving execution requires protection", ErrInvalid)
	}
	if err := h.admission.ValidateScope(config.Scope); err != nil {
		return nil, err
	}
	work, err := config.Owner.Enter(ctx)
	if err != nil {
		return nil, err
	}
	defer work.Finish()
	ctx = work.Context()
	binding, err := h.activate(ctx, config.Realm, recordID, nil, nil, config.Scope)
	if err != nil {
		return nil, err
	}
	return h.acquireReadyBinding(ctx, binding, config, recordID)
}

func (h *Host) acquireReadyBinding(ctx context.Context, binding BindingSnapshot, config UseConfig, recordID string) (*Use, error) {
	h.mu.Lock()
	plan := &BindingPlan{owner: h.identity, realm: config.Realm, binding: binding.Clone(), durable: true, members: []snapshotMember{}}
	for _, source := range binding.Roots {
		instance, err := h.readyInstanceLocked(source.Unit(), config.Realm)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		selected, err := instance.setupExportLocked(source)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		plan.members = append(plan.members, snapshotMember{id: selected.id, source: selected.source, instanceID: selected.instance.id, binding: selected.instance.snapshot.Clone(), durable: true})
	}
	for _, source := range binding.UnitRoots {
		instance, err := h.readyInstanceLocked(source, config.Realm)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		plan.unitMembers = append(plan.unitMembers, unitMember{source: source, instanceID: instance.id})
	}
	h.mu.Unlock()
	return h.acquirePrepared(ctx, plan, config, recordID)
}

func (h *Host) readyInstanceLocked(source UnitRef, requestedRealm RealmID) (*Instance, error) {
	unit, _, err := h.unitLocked(source)
	if err != nil {
		return nil, err
	}
	realm, err := h.realmLocked(unit.Declaration, requestedRealm)
	if err != nil {
		return nil, err
	}
	instance := h.instances[instanceKey{source: source, realm: realm}]
	if instance == nil || instance.retiring || !instance.readyDone || instance.readyErr != nil {
		return nil, ErrUnavailable
	}
	return instance, nil
}

func (h *Host) AcquireUnit(ctx context.Context, root UnitRef, config UseConfig) (*Use, error) {
	plan, err := h.PlanUnit(ctx, config.Realm, root, nil)
	if err != nil {
		return nil, err
	}
	return h.Acquire(ctx, plan, config)
}

func (u *Use) Sources() []ExportRef {
	return slices.Clone(u.binding.Roots)
}
