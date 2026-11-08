package frame

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
)

type UseConfig struct {
	Owner       *Lifetime
	Scope       ScopeGrant
	Parent      *Use
	Realm       RealmID
	ExecutionID string
}

// Use is one admitted borrow of an exact binding. It holds every instance it
// may reach until its own Lifetime has released, and a surviving execution's
// Use stays protected until its release has been durably settled.
type Use struct {
	authority   *authorityIdentity
	id          string
	parentID    string
	executionID string
	realm       RealmID
	binding     BindingSnapshot
	lifetime    *Lifetime
	protected   bool
	// initialized is closed once admission has published or refused the Use;
	// retirement reads no admission-written field before then.
	initialized chan struct{}
	settlement  *settlement

	// Host.mu guards the fields below. Retirement clears them only after the
	// Use's Lifetime has released, so work inside the Use may read them.
	host          *Host
	scope         ScopeGrant
	members       map[string]*publication
	selections    map[string][]string
	instances     []*unitInstance
	instanceHolds []*Work
	stops         []func() bool
	hostResource  *ResourceHandle
	ownerResource *ResourceHandle
}

const (
	useResourcePrefix   = "use:"
	useSettlementPrefix = "use settlement:"
)

func (u *Use) ID() string               { return u.id }
func (u *Use) Context() context.Context { return u.lifetime.Context() }
func (u *Use) Lifetime() *Lifetime      { return u.lifetime }
func (u *Use) Binding() BindingSnapshot { return u.binding.Clone() }
func (u *Use) Realm() RealmID           { return u.realm }

func (u *Use) Sources() []ExportRef {
	return slices.Clone(u.binding.Roots)
}

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

func (u *Use) instance(root UnitRef) *unitInstance {
	for _, instance := range u.instances {
		if instance.binding.Source == root {
			return instance
		}
	}
	return nil
}

// ValidateUse checks the original accepted permission. Withdrawal of a Point
// member does not rewrite this Use or require durable reconstruction identity.
func (h *Host) ValidateUse(ctx context.Context, use *Use) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if use == nil || use.authority != h.identity {
		return ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() || h.uses[use.id] != use || use.lifetime.isClosing() {
		return ErrClosed
	}
	return h.admission.ValidateScope(use.scope)
}

func (h *Host) Acquire(ctx context.Context, plan *BindingPlan, config UseConfig) (*Use, error) {
	if ctx == nil || plan == nil {
		return nil, ErrInvalid
	}
	config, err := h.useConfig(config)
	if err != nil {
		return nil, err
	}
	return h.acquirePlan(ctx, plan, config)
}

func (h *Host) acquirePlan(ctx context.Context, plan *BindingPlan, config UseConfig) (*Use, error) {
	if config.Realm != plan.realm {
		return nil, ErrContractMismatch
	}
	if plan.preparation == nil {
		return h.acquirePrepared(ctx, plan, config, "")
	}
	return h.acquireActivated(ctx, config, "", h.plannedBinding(plan))
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
	if ctx == nil || h.readAcceptedBinding == nil || !ValidName(recordID) {
		return nil, ErrInvalid
	}
	config, err := h.useConfig(config)
	if err != nil {
		return nil, err
	}
	return h.acquireActivated(ctx, config, recordID, h.acceptedBinding(recordID, config.Scope))
}

func (h *Host) AcquireUnit(ctx context.Context, root UnitRef, config UseConfig) (*Use, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	config, err := h.useConfig(config)
	if err != nil {
		return nil, err
	}
	plan, err := h.PlanUnit(ctx, config.Realm, root, nil)
	if err != nil {
		return nil, err
	}
	return h.acquirePlan(ctx, plan, config)
}

// useConfig completes a request from its parent. A child must remain inside
// its parent's execution, realm and scope; another owner would detach
// protected work from the permission that admitted it.
func (h *Host) useConfig(config UseConfig) (UseConfig, error) {
	if config.Owner == nil {
		return config, ErrInvalid
	}
	if config.Owner.authority != nil && config.Owner.authority != h.identity {
		return config, fmt.Errorf("%w: use owner belongs to another authority", ErrInvalid)
	}
	if parent := config.Parent; parent != nil {
		if parent.authority != h.identity || config.Owner != parent.lifetime {
			return config, ErrInvalid
		}
		inherited, err := h.inherit(config, parent)
		if err != nil {
			return config, err
		}
		config = inherited
	}
	if config.Realm != "" {
		if err := config.Realm.Validate(); err != nil {
			return config, err
		}
	}
	if config.ExecutionID != "" {
		if err := validateName("execution identity", config.ExecutionID); err != nil {
			return config, err
		}
	}
	if config.Realm != "" && h.protection == nil {
		return config, fmt.Errorf("%w: surviving execution requires protection", ErrInvalid)
	}
	return config, nil
}

func (h *Host) inherit(config UseConfig, parent *Use) (UseConfig, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() || h.uses[parent.id] != parent || parent.lifetime.isClosing() {
		return config, ErrClosed
	}
	if !config.Scope.issued() {
		config.Scope = parent.scope
	}
	config.Realm = cmp.Or(config.Realm, parent.realm)
	config.ExecutionID = cmp.Or(config.ExecutionID, parent.executionID)
	if config.ExecutionID != parent.executionID || config.Realm != parent.realm || config.Scope != parent.scope {
		return config, ErrInvalid
	}
	return config, nil
}

// acquireActivated prepares the graph inside the owner's work, so closing the
// owner cancels the preparation it is waiting for.
func (h *Host) acquireActivated(ctx context.Context, config UseConfig, recordID string, bind activationBinding) (*Use, error) {
	work, err := config.Owner.Enter(ctx)
	if err != nil {
		return nil, err
	}
	defer work.Finish()
	if err := h.admission.ValidateScope(config.Scope); err != nil {
		return nil, err
	}
	binding, err := h.activate(work.Context(), config.Realm, config.Scope, bind)
	if err != nil {
		return nil, err
	}
	plan, err := h.readyPlan(binding, config.Realm)
	if err != nil {
		return nil, err
	}
	return h.acquirePrepared(work.Context(), plan, config, recordID)
}

func (h *Host) readyPlan(binding BindingSnapshot, realm RealmID) (*BindingPlan, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	plan := &BindingPlan{owner: h.identity, realm: realm, binding: binding.Clone(), members: []snapshotMember{}}
	for _, source := range binding.Roots {
		instance, err := h.readyInstanceLocked(source.Unit(), realm)
		if err != nil {
			return nil, err
		}
		export, err := instance.setupExportLocked(source)
		if err != nil {
			return nil, err
		}
		plan.members = append(plan.members, snapshotMember{id: export.id, source: export.source, instanceID: instance.id, binding: instance.snapshot.Clone(), durable: true})
	}
	for _, source := range binding.UnitRoots {
		instance, err := h.readyInstanceLocked(source, realm)
		if err != nil {
			return nil, err
		}
		plan.unitMembers = append(plan.unitMembers, unitMember{source: source, instanceID: instance.id})
	}
	return plan, nil
}

func (h *Host) readyInstanceLocked(source UnitRef, realm RealmID) (*unitInstance, error) {
	unit, _, err := h.unitLocked(source)
	if err != nil {
		return nil, err
	}
	realm, err = h.realmLocked(unit.Declaration, realm)
	if err != nil {
		return nil, err
	}
	instance := h.instances[instanceKey{source: source, realm: realm}]
	if instance == nil || !instance.servingLocked() {
		return nil, ErrUnavailable
	}
	return instance, nil
}

func (h *Host) acquirePrepared(ctx context.Context, plan *BindingPlan, config UseConfig, recordID string) (_ *Use, err error) {
	use := &Use{
		authority:   h.identity,
		id:          rand.Text(),
		executionID: config.ExecutionID,
		scope:       config.Scope,
		realm:       config.Realm,
		binding:     plan.binding.Clone(),
		lifetime:    h.newLifetime(context.WithoutCancel(ctx)),
		initialized: make(chan struct{}),
		settlement:  newSettlement(),
		host:        h,
		members:     make(map[string]*publication),
		selections:  make(map[string][]string),
	}
	published := false
	defer func() {
		close(use.initialized)
		if !published {
			use.BeginClose(err)
		}
	}()
	causes := use.lifetime.ObserveCloseCauses()
	context.AfterFunc(use.Context(), func() {
		<-use.initialized
		operation := use.lifetime.BeginClose(ErrClosed)
		// This join cannot be cancelled; cleanup diagnostics cannot invalidate its proof.
		outcome, _ := operation.WaitReleased(context.Background())
		proof, _ := operation.ReleaseProof()
		use.settleRelease(proof)
		use.retire(causes.join(outcomeError(outcome)))
		use.settlement.complete()
	})
	if config.Parent != nil {
		use.parentID = config.Parent.id
	}
	err = h.admission.Admit(ctx, func(admitted context.Context) error {
		if err := h.admission.ValidateScope(config.Scope); err != nil {
			return err
		}
		if recordID != "" {
			if err := h.checkAcceptedBinding(admitted, recordID, config.Scope, use.binding); err != nil {
				return err
			}
		}
		if err := h.attachPlanMembers(use, plan, config.Owner); err != nil {
			return err
		}
		if err := h.reserveUse(use, config.Owner); err != nil {
			return err
		}
		if err := h.protectUse(admitted, use); err != nil {
			return err
		}
		if config.Owner.isClosing() || h.lifetime.isClosing() || use.lifetime.isClosing() {
			return ErrClosed
		}
		if err := contextError(admitted); err != nil {
			return err
		}
		return h.admission.RecoveryError()
	})
	if err != nil {
		return nil, err
	}
	published = true
	return use, nil
}

func (h *Host) checkAcceptedBinding(admitted context.Context, recordID string, scope ScopeGrant, binding BindingSnapshot) error {
	accepted, err := h.readAcceptedBinding(admitted, recordID, scope)
	if err != nil {
		return err
	}
	same, err := binding.sameIdentity(accepted)
	if err != nil {
		return err
	}
	if !same {
		return ErrStaleBinding
	}
	return nil
}

func (h *Host) attachPlanMembers(use *Use, plan *BindingPlan, owner *Lifetime) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.validatePlanLocked(plan); err != nil {
		return err
	}
	if len(h.uses) >= h.limits.MaxUses {
		return fmt.Errorf("%w: active uses", ErrCapacity)
	}
	attached := make(map[*unitInstance]bool)
	attach := func(instance *unitInstance) {
		if !attached[instance] {
			attached[instance] = true
			use.instances = append(use.instances, instance)
		}
	}
	for _, member := range plan.members {
		actual := h.catalog.member(member.source.Contract, member.id)
		use.members[member.id] = actual
		attach(actual.instance)
	}
	for _, member := range plan.unitMembers {
		attach(h.allInstances[member.instanceID])
	}
	if err := h.validateUseOwnerLocked(owner, use.instances); err != nil {
		return err
	}
	for _, selection := range plan.selections {
		ids := make([]string, 0, len(selection.members))
		for _, member := range selection.members {
			ids = append(ids, member.id)
		}
		use.selections[selection.id] = ids
	}
	for _, instance := range use.instances {
		if err := use.holdLocked(instance, plan.binding); err != nil {
			return err
		}
	}
	return nil
}

// holdLocked keeps instance alive for this Use. An instance may have retired
// and been rebuilt since capture; the Use only accepts the setup binding that
// was originally admitted for that particular instance.
func (u *Use) holdLocked(instance *unitInstance, graph BindingSnapshot) error {
	_, identity, err := setupBinding(graph, instance.snapshot.Roots, instance.binding.Source)
	if err != nil {
		return errors.Join(ErrStaleBinding, err)
	}
	if identity != instance.identity {
		return fmt.Errorf("%w: unit acquired another setup binding", ErrStaleBinding)
	}
	if instance.startGrant.issued() && instance.startGrant != u.scope {
		return ErrFenced
	}
	hold, err := instance.lifetime.hold()
	if err != nil {
		return err
	}
	u.instanceHolds = append(u.instanceHolds, hold)
	u.stops = append(u.stops, context.AfterFunc(hold.Context(), func() { u.BeginClose(context.Cause(hold.Context())) }))
	return nil
}

// Ownership and held-provider edges come from the original resources. A second
// dependency ledger could disagree after a Use closes or changes admission.
func (h *Host) validateUseOwnerLocked(owner *Lifetime, instances []*unitInstance) error {
	dependencies := make(map[*Lifetime][]*Lifetime)
	for _, instance := range h.allInstances {
		for _, provider := range instance.providers {
			dependencies[instance.lifetime] = append(dependencies[instance.lifetime], provider.lifetime)
		}
	}
	for _, use := range h.uses {
		if use.ownerResource == nil {
			continue
		}
		originalOwner := use.ownerResource.lifetime
		dependencies[originalOwner] = append(dependencies[originalOwner], use.lifetime)
		for _, instance := range use.instances {
			dependencies[use.lifetime] = append(dependencies[use.lifetime], instance.lifetime)
		}
	}
	pending := make([]*Lifetime, 0, len(instances))
	for _, instance := range instances {
		pending = append(pending, instance.lifetime)
	}
	visited := make(map[*Lifetime]bool)
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == owner {
			return fmt.Errorf("%w: use owner depends on its held instances", ErrDependencyCycle)
		}
		if visited[current] {
			continue
		}
		visited[current] = true
		pending = append(pending, dependencies[current]...)
	}
	return nil
}

// protectUse records a surviving execution's Use approval before it is
// published, so a restart can still account for it.
func (h *Host) protectUse(admitted context.Context, use *Use) error {
	if use.realm == "" {
		return nil
	}
	instanceIDs := make([]string, 0, len(use.instances))
	for _, instance := range use.instances {
		instanceIDs = append(instanceIDs, instance.id)
	}
	slices.Sort(instanceIDs)
	approval := UseApproval{ID: use.id, ParentID: use.parentID, ExecutionID: use.executionID, Instances: instanceIDs, Realm: use.realm, Scope: use.scope.WireID(), ScopeGeneration: use.scope.Generation(), Epoch: h.epoch, Binding: use.binding.Clone()}
	if err := approval.Validate(); err != nil {
		return err
	}
	concluded := false
	defer func() {
		if !concluded {
			h.admission.RequireRecovery(fmt.Errorf("%w: use approval interrupted", ErrRecoveryRequired))
		}
	}()
	err := h.protection.ApproveUse(admitted, approval)
	concluded = true
	if err != nil {
		return err
	}
	use.protected = true
	return nil
}

// The original Use must remain reachable if approval succeeds but publication
// fails. Both lifetimes reserve its cleanup before durable protection begins.
func (h *Host) reserveUse(use *Use, owner *Lifetime) error {
	causes := use.lifetime.ObserveCloseCauses()
	resource := Resource{
		Name:   useResourcePrefix + use.id,
		Cancel: func(cause error) { use.BeginClose(cause) },
		Close: func(ctx context.Context) (ReleaseState, error) {
			outcome, err := use.close(ctx, use.settlement.observe())
			return releaseState(outcome, causes.join(err))
		},
	}
	instances := make([]*ResourceHandle, 0, len(use.instances))
	for _, instance := range use.instances {
		instances = append(instances, instance.resource)
	}
	var ownerDependencies []*ResourceHandle
	if owner == h.lifetime {
		ownerDependencies = instances
	} else {
		hostResource, err := h.lifetime.Own(resource, instances...)
		if err != nil {
			return err
		}
		use.hostResource = hostResource
	}
	ownerResource, err := owner.Own(resource, ownerDependencies...)
	if err != nil {
		return err
	}
	use.ownerResource = ownerResource
	h.mu.Lock()
	defer h.mu.Unlock()
	h.uses[use.id] = use
	// An installation fence retires the held instances, which close the Use.
	if use.scope.key.namespace == applicationNamespace {
		use.stops = append(use.stops, context.AfterFunc(use.scope.context, func() { use.BeginClose(ErrFenced) }))
	}
	return nil
}

func (u *Use) BeginClose(cause error) *Close {
	return u.lifetime.BeginClose(cause)
}

// settleRelease durably releases a protected Use. A failed attempt fences new
// admission and only an explicit Use or Host close can request another attempt.
func (u *Use) settleRelease(proof ReleaseProof) {
	h := u.host
	for u.protected {
		err := joinCallback("use release", func() error {
			return h.protection.ReleaseUse(context.Background(), u.id, proof)
		})
		if err == nil {
			return
		}
		h.admission.RequireRecovery(err)
		u.settlement.fail(err)
		u.settlement.awaitRetry()
	}
}

func (u *Use) retire(diagnostic error) {
	for _, stop := range u.stops {
		stop()
	}
	for _, hold := range u.instanceHolds {
		hold.Finish()
	}
	if u.ownerResource != nil {
		_ = u.ownerResource.ConfirmReleased(diagnostic)
	}
	if u.hostResource != nil {
		_ = u.hostResource.ConfirmReleased(diagnostic)
	}
	h := u.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uses[u.id] == u {
		delete(h.uses, u.id)
	}
	u.members = nil
	u.selections = nil
	u.instances = nil
	u.instanceHolds = nil
	u.stops = nil
	u.hostResource = nil
	u.ownerResource = nil
	u.scope = ScopeGrant{}
	u.host = nil
}

func (u *Use) Close(ctx context.Context) (CloseOutcome, error) {
	if ctx == nil || u == nil {
		return CloseOutcome{}, ErrInvalid
	}
	reports, _ := u.settlement.requestRetry()
	return u.close(ctx, reports)
}

func (u *Use) close(ctx context.Context, reports uint64) (CloseOutcome, error) {
	causes := u.lifetime.ObserveCloseCauses()
	outcome, err := u.BeginClose(ErrClosed).Wait(ctx)
	err = causes.join(err)
	if !outcome.Released {
		return outcome, err
	}
	if settleErr := u.settlement.await(ctx, reports); settleErr != nil {
		outcome.Released = false
		outcome.Pending = append(outcome.Pending, useSettlementPrefix+u.id)
		return outcome, errors.Join(err, settleErr)
	}
	return outcome, err
}

// WaitRetired joins the original exit, including stable execution-protection
// settlement and removal of Host references. It never starts a second cleanup.
func (u *Use) WaitRetired(ctx context.Context) (CloseOutcome, error) {
	if ctx == nil || u == nil {
		return CloseOutcome{}, ErrInvalid
	}
	select {
	case <-u.settlement.done:
		outcome, _ := u.lifetime.closeOutcome()
		return outcome, outcomeError(outcome)
	case <-ctx.Done():
		return CloseOutcome{}, contextError(ctx)
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
		for i, member := range members {
			values[i] = member.Value
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
	value, found := exportValue[V](use, source)
	if !found {
		return ErrContractMismatch
	}
	return call(work.Context(), value)
}

func exportValue[V any](use *Use, source ExportRef) (V, bool) {
	use.host.mu.Lock()
	defer use.host.mu.Unlock()
	for _, member := range use.members {
		if member.source == source {
			value, ok := member.value.(V)
			return value, ok
		}
	}
	var zero V
	return zero, false
}

func WithPlan(ctx context.Context, host *Host, plan *BindingPlan, scope ScopeGrant, call func(*Use) error) (err error) {
	if ctx == nil || host == nil || plan == nil || call == nil {
		return ErrInvalid
	}
	owner := NewLifetime(context.WithoutCancel(ctx))
	defer func() {
		_, closeErr := owner.Close(context.Background())
		err = errors.Join(err, closeErr)
	}()
	use, err := host.Acquire(ctx, plan, UseConfig{Owner: owner, Scope: scope, Realm: plan.realm})
	if err != nil {
		return err
	}
	return call(use)
}
