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
	instances     []*Instance
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

func (u *Use) instance(root UnitRef) *Instance {
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
	if ctx == nil || plan == nil || config.Owner == nil {
		return nil, ErrInvalid
	}
	config, err := h.useConfig(config)
	if err != nil {
		return nil, err
	}
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
	if ctx == nil || config.Owner == nil || h.readAcceptedBinding == nil || !ValidName(recordID) {
		return nil, ErrInvalid
	}
	config, err := h.useConfig(config)
	if err != nil {
		return nil, err
	}
	return h.acquireActivated(ctx, config, recordID, h.acceptedBinding(recordID, config.Scope))
}

func (h *Host) AcquireUnit(ctx context.Context, root UnitRef, config UseConfig) (*Use, error) {
	plan, err := h.PlanUnit(ctx, config.Realm, root, nil)
	if err != nil {
		return nil, err
	}
	return h.Acquire(ctx, plan, config)
}

// useConfig completes a request from its parent. A child must remain inside
// its parent's execution, realm and scope; another owner would detach
// protected work from the permission that admitted it.
func (h *Host) useConfig(config UseConfig) (UseConfig, error) {
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
	if h.lifetime.isClosing() || h.uses[parent.id] != parent {
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

func (h *Host) readyInstanceLocked(source UnitRef, realm RealmID) (*Instance, error) {
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
		lifetime:    NewLifetime(context.WithoutCancel(ctx)),
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
		if err := h.protectUse(admitted, use); err != nil {
			return err
		}
		return h.publishUse(use, config.Owner)
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
	attached := make(map[*Instance]bool)
	attach := func(instance *Instance) {
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
func (u *Use) holdLocked(instance *Instance, graph BindingSnapshot) error {
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

// An instance drains its consumers before closing owned resources. Accepting a
// Use owned by that instance, even through another Use, would make both wait for
// the other. Follow the original resource handles and provider edges only.
func (h *Host) validateUseOwnerLocked(owner *Lifetime, instances []*Instance) error {
	owners := make(map[*Lifetime]bool)
	for owner != nil && !owners[owner] {
		owners[owner] = true
		owner = h.useOwnerLocked(owner)
	}
	visited := make(map[*Instance]bool)
	pending := slices.Clone(instances)
	for len(pending) > 0 {
		instance := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[instance] {
			continue
		}
		if owners[instance.lifetime] {
			return fmt.Errorf("%w: use owner protects instance %s", ErrDependencyCycle, instance.id)
		}
		visited[instance] = true
		pending = append(pending, instance.providers...)
	}
	return nil
}

func (h *Host) useOwnerLocked(l *Lifetime) *Lifetime {
	for _, use := range h.uses {
		if use.lifetime == l && use.ownerResource != nil {
			return use.ownerResource.lifetime
		}
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

// publishUse makes the Use a resource of both its owner and the Host, the
// latter after every instance it holds, so Host closure drains it first.
func (h *Host) publishUse(use *Use, owner *Lifetime) error {
	causes := use.lifetime.ObserveCloseCauses()
	resource := Resource{
		Name:   useResourcePrefix + use.id,
		Cancel: func(cause error) { use.BeginClose(cause) },
		Close: func(ctx context.Context) (ReleaseState, error) {
			outcome, err := use.Close(ctx)
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
	}
	ownerResource, err := owner.Own(resource, ownerDependencies...)
	if err != nil {
		return err
	}
	use.ownerResource = ownerResource
	if owner != h.lifetime {
		hostResource, err := h.lifetime.Own(resource, instances...)
		if err != nil {
			return err
		}
		use.hostResource = hostResource
	}
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
// admission and is retried only when a caller explicitly closes the Use again.
func (u *Use) settleRelease(proof ReleaseProof) {
	h := u.host
	for u.protected {
		err := h.protection.ReleaseUse(context.Background(), u.id, proof)
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
	causes := u.lifetime.ObserveCloseCauses()
	outcome, err := u.BeginClose(ErrClosed).Wait(ctx)
	err = causes.join(err)
	if !outcome.Released {
		return outcome, err
	}
	if settleErr := u.settlement.await(ctx); settleErr != nil {
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
