package host

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type UseConfig struct {
	Owner       frame.Lifetime
	Scope       ScopeGrant
	Parent      Use
	Realm       RealmID
	ExecutionID string
}

// Use is one admitted borrow of an exact binding. It holds every instance it
// may reach until its own Lifetime has released, and a surviving execution's
// Use stays protected until its release has been durably settled.
type Use struct {
	borrow *useBorrow
}

// Copies retain the original borrow; only its retirement clears held objects.
type useBorrow struct {
	authority       *frame.LifetimeDomain
	id              string
	parentID        string
	executionID     string
	realm           RealmID
	binding         BindingSnapshot
	lifetime        frame.Lifetime
	approvalStarted bool
	// initialized is closed once admission has published or refused the Use;
	// retirement reads no admission-written field before then.
	initialized chan struct{}
	settlement  *settlement

	// Host.mu guards the fields below. Retirement clears them only after the
	// Use's Lifetime has released, so work inside the Use may read them.
	host          *hostState
	owner         frame.Lifetime
	scope         ScopeGrant
	members       map[string]*publication
	selections    map[string][]string
	instanceHolds heldInstances
	stops         []func() bool
	hostResource  frame.ResourceHandle
	ownerResource frame.ResourceHandle
}

const (
	useResourcePrefix   = "use:"
	useSettlementPrefix = "use settlement:"
)

// Valid reports whether the handle names an admitted borrow; the zero Use does not.
func (u Use) Valid() bool { return u.borrow != nil }

func (u Use) ID() string {
	if u.borrow == nil {
		return ""
	}
	return u.borrow.id
}

func (u Use) Context() context.Context {
	if u.borrow == nil {
		return nil
	}
	return u.borrow.lifetime.Context()
}

func (u Use) Lifetime() frame.Lifetime {
	if u.borrow == nil {
		return frame.Lifetime{}
	}
	return u.borrow.lifetime
}

func (u Use) Binding() BindingSnapshot {
	if u.borrow == nil {
		return BindingSnapshot{}
	}
	return u.borrow.binding.Clone()
}

func (u Use) Realm() RealmID {
	if u.borrow == nil {
		return ""
	}
	return u.borrow.realm
}

func (u Use) Sources() []ExportRef {
	if u.borrow == nil {
		return nil
	}
	return slices.Clone(u.borrow.binding.Roots)
}

// Enter retains this Use's resources and checks its original grant before new
// work begins, even while asynchronous retirement has not observed a fence.
func (u Use) Enter(ctx context.Context) (frame.Work, error) {
	if u.borrow == nil {
		return frame.Work{}, frame.ErrInvalid
	}
	return u.borrow.enter(ctx)
}

func (u *useBorrow) enter(ctx context.Context) (frame.Work, error) {
	work, err := u.lifetime.Enter(ctx)
	if err != nil {
		return frame.Work{}, err
	}
	if err := u.validate(); err != nil {
		work.Finish()
		return frame.Work{}, err
	}
	return work, nil
}

// Cancellation notifications start cleanup; admission reads the original
// owners directly. A draining instance can still serve its accepted Uses.
func (u *useBorrow) validate() error {
	if u.lifetime.IsClosing() || u.host.lifetime.IsClosing() || u.owner.IsClosing() {
		return frame.ErrClosed
	}
	if err := u.host.admission.ValidateScope(u.scope); err != nil {
		return err
	}
	return u.instanceHolds.validate()
}

// ValidateUse checks the original accepted permission. Withdrawal of a Point
// member does not rewrite this Use or require durable reconstruction identity.
func (h *hostState) ValidateUse(ctx context.Context, use Use) error {
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	if !use.Valid() {
		return frame.ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.validateUseLocked(use.borrow)
}

func (h *hostState) validateUseLocked(use *useBorrow) error {
	if use == nil || use.authority != h.identity {
		return frame.ErrInvalid
	}
	if h.uses[use.id] != use {
		return frame.ErrClosed
	}
	return use.validate()
}

func (h *hostState) Acquire(ctx context.Context, plan BindingPlan, config UseConfig) (Use, error) {
	if framevalue.IsNil(ctx) || plan.plan == nil {
		return Use{}, frame.ErrInvalid
	}
	config, err := h.useConfig(config)
	if err != nil {
		return Use{}, err
	}
	return h.acquirePlan(ctx, plan.plan, config)
}

func (h *hostState) acquirePlan(ctx context.Context, plan admissionPlan, config UseConfig) (Use, error) {
	if config.Realm != plan.header().realm {
		return Use{}, frame.ErrContractMismatch
	}
	return plan.acquire(ctx, h, config)
}

func (h *hostState) AcquireChild(ctx context.Context, parent Use, plan BindingPlan) (Use, error) {
	if parent.borrow == nil {
		return Use{}, frame.ErrInvalid
	}
	return h.Acquire(ctx, plan, UseConfig{Owner: parent.borrow.lifetime, Parent: parent})
}

// AcquireRecord restores the original owner's committed graph without rerunning
// selection. A serialized plan snapshot alone cannot authorize this path.
func (h *hostState) AcquireRecord(ctx context.Context, recordID string, config UseConfig) (Use, error) {
	if framevalue.IsNil(ctx) || h.readAcceptedBinding == nil || !frame.ValidName(recordID) {
		return Use{}, frame.ErrInvalid
	}
	config, err := h.useConfig(config)
	if err != nil {
		return Use{}, err
	}
	return h.acquireActivated(ctx, config, recordID, h.acceptedBinding(recordID, config.Scope))
}

func (h *hostState) AcquireUnit(ctx context.Context, root UnitRef, config UseConfig) (Use, error) {
	if framevalue.IsNil(ctx) {
		return Use{}, frame.ErrInvalid
	}
	config, err := h.useConfig(config)
	if err != nil {
		return Use{}, err
	}
	plan, err := h.plan(ctx, config.Realm, nil, nil, []UnitRef{root})
	if err != nil {
		return Use{}, err
	}
	return h.acquirePlan(ctx, plan, config)
}

// useConfig completes a request from its parent. A child must remain inside
// its parent's execution, realm and scope; another owner would detach
// protected work from the permission that admitted it.
func (h *hostState) useConfig(config UseConfig) (UseConfig, error) {
	if !config.Owner.Valid() {
		return config, frame.ErrInvalid
	}
	if config.Owner.HasDomain() && !h.identity.Owns(config.Owner) {
		return config, fmt.Errorf("%w: use owner belongs to another authority", frame.ErrInvalid)
	}
	if parent := config.Parent; parent.Valid() {
		if parent.borrow.authority != h.identity || config.Owner != parent.borrow.lifetime {
			return config, frame.ErrInvalid
		}
		inherited, err := h.inherit(config, parent.borrow)
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
	if config.Realm != "" && h.durable == nil {
		return config, fmt.Errorf("%w: surviving execution requires a durable authority", frame.ErrInvalid)
	}
	return config, nil
}

func (h *hostState) inherit(config UseConfig, parent *useBorrow) (UseConfig, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.IsClosing() || h.uses[parent.id] != parent || parent.lifetime.IsClosing() {
		return config, frame.ErrClosed
	}
	if !config.Scope.issued() {
		config.Scope = parent.scope
	}
	config.Realm = cmp.Or(config.Realm, parent.realm)
	config.ExecutionID = cmp.Or(config.ExecutionID, parent.executionID)
	if config.ExecutionID != parent.executionID || config.Realm != parent.realm || config.Scope != parent.scope {
		return config, frame.ErrInvalid
	}
	return config, nil
}

// acquireActivated prepares the graph inside the owner's work, so closing the
// owner cancels the preparation it is waiting for.
func (h *hostState) acquireActivated(ctx context.Context, config UseConfig, recordID string, bind activationBinding) (Use, error) {
	work, err := config.Owner.Enter(ctx)
	if err != nil {
		return Use{}, err
	}
	defer work.Finish()
	if err := h.admission.ValidateScope(config.Scope); err != nil {
		return Use{}, err
	}
	plan, err := h.activate(work.Context(), config.Realm, config.Scope, bind)
	if err != nil {
		return Use{}, err
	}
	return h.acquirePrepared(work.Context(), plan, config, recordID)
}

func (h *hostState) acquirePrepared(ctx context.Context, plan *capturedPlan, config UseConfig, recordID string) (_ Use, err error) {
	use := &useBorrow{
		authority:   h.identity,
		id:          rand.Text(),
		executionID: config.ExecutionID,
		scope:       config.Scope,
		realm:       config.Realm,
		binding:     plan.binding.Clone(),
		lifetime:    h.newLifetime(context.WithoutCancel(ctx)),
		owner:       config.Owner,
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
			use.lifetime.BeginClose(err)
		}
	}()
	causes := use.lifetime.ObserveCloseCauses()
	context.AfterFunc(use.lifetime.Context(), func() { use.retireAfterClose(causes) })
	if config.Parent.Valid() {
		use.parentID = config.Parent.borrow.id
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
		if err := h.admission.ValidateScope(config.Scope); err != nil {
			return err
		}
		if err := h.attachPlanMembers(use, plan); err != nil {
			return err
		}
		if err := h.reserveUse(use); err != nil {
			return err
		}
		if err := h.protectUse(admitted, use); err != nil {
			return err
		}
		if err := use.validate(); err != nil {
			return err
		}
		if err := frame.ContextError(admitted); err != nil {
			return err
		}
		return h.admission.RecoveryError()
	})
	if err != nil {
		return Use{}, err
	}
	published = true
	return Use{borrow: use}, nil
}

func (h *hostState) checkAcceptedBinding(admitted context.Context, recordID string, scope ScopeGrant, binding BindingSnapshot) error {
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

func (h *hostState) attachPlanMembers(use *useBorrow, plan *capturedPlan) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.validatePlanLocked(plan); err != nil {
		return err
	}
	if len(h.uses) >= h.limits.MaxUses {
		return fmt.Errorf("%w: active uses", frame.ErrCapacity)
	}
	attached := make(map[*unitInstance]bool)
	var instances []*unitInstance
	attach := func(instance *unitInstance) {
		if !attached[instance] {
			attached[instance] = true
			instances = append(instances, instance)
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
	if err := h.validateUseOwnerLocked(use.owner, instances); err != nil {
		return err
	}
	for _, selection := range plan.selections {
		ids := make([]string, 0, len(selection.members))
		for _, member := range selection.members {
			ids = append(ids, member.id)
		}
		use.selections[selection.id] = ids
	}
	for _, instance := range instances {
		if err := use.holdLocked(instance, plan.binding); err != nil {
			return err
		}
	}
	return use.validate()
}

// A captured instance must keep its original setup graph; an export name
// cannot authorize a different provider binding.
func (u *useBorrow) holdLocked(instance *unitInstance, graph BindingSnapshot) error {
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
	hold, err := instance.hold()
	if err != nil {
		return err
	}
	u.instanceHolds = append(u.instanceHolds, hold)
	u.stops = append(u.stops, hold.onInstanceCancel(func(cause error) { u.lifetime.BeginClose(cause) }))
	return nil
}

// Ownership and held-provider edges come from the original resources. A second
// dependency ledger could disagree after a Use closes or changes admission.
func (h *hostState) validateUseOwnerLocked(owner frame.Lifetime, instances []*unitInstance) error {
	dependencies := make(map[frame.LifetimeID][]frame.LifetimeID)
	for _, instance := range h.allInstances {
		for _, hold := range instance.providerHolds {
			dependencies[instance.lifetime.Identity()] = append(dependencies[instance.lifetime.Identity()], hold.instance.lifetime.Identity())
		}
	}
	for _, use := range h.uses {
		originalOwner := use.ownerResource.OwnerID()
		dependencies[originalOwner] = append(dependencies[originalOwner], use.lifetime.Identity())
		for _, hold := range use.instanceHolds {
			dependencies[use.lifetime.Identity()] = append(dependencies[use.lifetime.Identity()], hold.instance.lifetime.Identity())
		}
	}
	pending := make([]frame.LifetimeID, 0, len(instances))
	for _, instance := range instances {
		pending = append(pending, instance.lifetime.Identity())
	}
	visited := make(map[frame.LifetimeID]bool)
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == owner.Identity() {
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
func (h *hostState) protectUse(admitted context.Context, use *useBorrow) error {
	if use.realm == "" {
		return nil
	}
	instanceIDs := make([]string, 0, len(use.instanceHolds))
	for _, hold := range use.instanceHolds {
		instanceIDs = append(instanceIDs, hold.instance.id)
	}
	slices.Sort(instanceIDs)
	approval := UseApproval{ID: use.id, ParentID: use.parentID, ExecutionID: use.executionID, Instances: instanceIDs, Realm: use.realm, Scope: use.scope.WireID(), ScopeGeneration: use.scope.Generation(), Epoch: h.Epoch(), Binding: use.binding.Clone()}
	if err := approval.Validate(); err != nil {
		return err
	}
	concluded := false
	defer func() {
		if !concluded {
			h.admission.RequireRecovery(fmt.Errorf("%w: use approval interrupted", ErrRecoveryRequired))
		}
	}()
	use.approvalStarted = true
	err := h.durable.ApproveUse(admitted, approval)
	concluded = true
	if err != nil {
		h.admission.RequireRecovery(err)
		return err
	}
	return nil
}

// The original Use must remain reachable if approval succeeds but publication
// fails. Both lifetimes reserve its cleanup before durable protection begins.
func (h *hostState) reserveUse(use *useBorrow) error {
	instances := make([]frame.ResourceHandle, 0, len(use.instanceHolds))
	for _, hold := range use.instanceHolds {
		instances = append(instances, hold.instance.resource)
	}
	var ownerDependencies []frame.ResourceHandle
	if use.owner == h.lifetime {
		ownerDependencies = instances
	} else {
		hostResource, err := use.reserveOwner(h.lifetime, instances...)
		if err != nil {
			return err
		}
		use.hostResource = hostResource
	}
	ownerResource, err := use.reserveOwner(use.owner, ownerDependencies...)
	if err != nil {
		return err
	}
	use.ownerResource = ownerResource
	h.mu.Lock()
	defer h.mu.Unlock()
	h.uses[use.id] = use
	use.stops = append(use.stops, context.AfterFunc(use.scope.context, func() { use.lifetime.BeginClose(ErrFenced) }))
	return nil
}

// Parent cancellation revokes permission even before its resource-close
// callbacks run. The reservation still waits for this Use's actual settlement.
func (u *useBorrow) reserveOwner(owner frame.Lifetime, dependencies ...frame.ResourceHandle) (frame.ResourceHandle, error) {
	causes := u.lifetime.ObserveCloseCauses()
	resource, err := owner.Own(frame.Resource{
		Name:   useResourcePrefix + u.id,
		Cancel: func(cause error) { u.lifetime.BeginClose(cause) },
		Close: func(ctx context.Context) (frame.ReleaseState, error) {
			outcome, err := u.close(ctx, u.settlement.observe())
			return outcome.ResourceState(causes.Join(err))
		},
	}, dependencies...)
	if err != nil {
		return frame.ResourceHandle{}, err
	}
	ctx := owner.Context()
	u.stops = append(u.stops, context.AfterFunc(ctx, func() { u.lifetime.BeginClose(context.Cause(ctx)) }))
	return resource, nil
}

func Borrow[M, V any](ctx context.Context, use Use, selection Selection[M, V], call func(context.Context, []V) error) error {
	if call == nil {
		return frame.ErrInvalid
	}
	return withAccess(ctx, use, func(a Access) error {
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

func BorrowExport[V any](ctx context.Context, use Use, source ExportRef, call func(context.Context, V) error) error {
	if use.borrow == nil || call == nil {
		return frame.ErrInvalid
	}
	borrow := use.borrow
	work, err := borrow.enter(ctx)
	if err != nil {
		return err
	}
	defer work.Finish()
	value, found := exportValue[V](borrow, source)
	if !found {
		return frame.ErrContractMismatch
	}
	return call(work.Context(), value)
}

func exportValue[V any](use *useBorrow, source ExportRef) (V, bool) {
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

func WithPlan(ctx context.Context, host Host, plan BindingPlan, scope ScopeGrant, call func(Use) error) (err error) {
	if framevalue.IsNil(ctx) || !host.Valid() || plan.plan == nil || call == nil {
		return frame.ErrInvalid
	}
	owner := frame.NewLifetime(context.WithoutCancel(ctx))
	defer func() {
		_, closeErr := owner.Close(context.Background())
		err = errors.Join(err, closeErr)
	}()
	use, err := host.Acquire(ctx, plan, UseConfig{Owner: owner, Scope: scope, Realm: plan.plan.header().realm})
	if err != nil {
		return err
	}
	return call(use)
}
