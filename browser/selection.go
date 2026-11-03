package browser

import (
	"context"
	"slices"

	"github.com/Tangerg/frame"
)

func (b *Bridge) Acquire(ctx context.Context, token string, request AcquireRequest) (grant UseGrant, returned error) {
	session, err := b.liveSession(token)
	if err != nil {
		return UseGrant{}, err
	}
	if !frame.ValidName(request.OperationID) {
		return UseGrant{}, frame.ErrInvalid
	}
	digest, err := requestDigest(request)
	if err != nil {
		return UseGrant{}, err
	}
	identity := controlRequestIdentity{Kind: acquireOperation, Realm: session.realm, Digest: digest}
	var entry *browserExecution
	var lease *snapshotLease
	operation, replay, err := b.begin(ctx, request.OperationID, identity, func(operation *controlOperation) error {
		entry, lease = b.executions.get(request.Execution.ID), b.snapshots[request.SnapshotID]
		if session.cleanupOnly {
			return errRealmSealed
		}
		if !entry.servesLocked(session.realm, request.Execution) || !lease.capturedFor(session.realm) {
			return frame.ErrStaleBinding
		}
		operation.execution = entry.plan.Execution.ID
		operation.parentID = request.ParentUseID
		return nil
	})
	if err != nil {
		return UseGrant{}, err
	}
	if replay {
		if err := operation.repeats(identity); err != nil {
			return UseGrant{}, err
		}
		return b.awaitGrant(ctx, operation)
	}
	defer func() { returned = b.finish(ctx, request.OperationID, operation, returned) }()
	if err := b.reserveReceipt(ctx, request.OperationID, operation); err != nil {
		return UseGrant{}, err
	}
	if err := b.takeLease(session, entry, request.SnapshotID, lease); err != nil {
		return UseGrant{}, err
	}
	plan, err := lease.bind(request.Selections)
	if err != nil {
		return UseGrant{}, err
	}
	config, err := b.useConfig(session, entry, request.ParentUseID)
	if err != nil {
		return UseGrant{}, err
	}
	use, err := b.config.Host.Acquire(ctx, plan, config)
	if err != nil {
		return UseGrant{}, err
	}
	return b.deliverUse(ctx, session, entry, use, request.OperationID, operation)
}

// takeLease consumes the Snapshot lease exactly once, including for an
// acquisition whose selection later fails.
func (b *Bridge) takeLease(session *realmSession, entry *browserExecution, id string, lease *snapshotLease) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session.cleanupOnly || entry.retiring || b.executions.get(entry.plan.Execution.ID) != entry || b.snapshots[id] != lease {
		return frame.ErrStaleBinding
	}
	delete(b.snapshots, id)
	return nil
}

func (b *Bridge) useConfig(session *realmSession, entry *browserExecution, parentID string) (frame.UseConfig, error) {
	if session.application == nil {
		return frame.UseConfig{}, frame.ErrFenced
	}
	// The Realm application consumes these exports. Making the provider execution
	// own that borrow would make its exit wait on its own resource protection.
	config := frame.UseConfig{Owner: session.application, Scope: entry.setup.ScopeGrant(), Realm: RealmID(session.realm), ExecutionID: entry.plan.Execution.ID}
	if parentID != "" {
		b.mu.Lock()
		parent := b.uses[parentID]
		valid := parent != nil && parent.grant.Realm == session.realm && parent.grant.Execution == entry.plan.Execution && parent.grant.ScopeID == entry.plan.ScopeID && parent.grant.ScopeGeneration == entry.plan.ScopeGeneration && !parent.released()
		b.mu.Unlock()
		if !valid {
			return frame.UseConfig{}, frame.ErrStaleBinding
		}
		config.Parent = parent.use
		config.Owner = parent.use.Lifetime()
	}
	return config, nil
}

func (b *Bridge) deliverUse(ctx context.Context, session *realmSession, entry *browserExecution, use *frame.Use, operationID string, operation *controlOperation) (UseGrant, error) {
	grant := UseGrant{OperationID: operationID, ParentUseID: operation.parentID, ID: use.ID(), ApprovalToken: b.signer.approval(use.ID()), Authority: b.authority, Realm: session.realm, Execution: entry.plan.Execution, ScopeID: entry.plan.ScopeID, ScopeGeneration: entry.plan.ScopeGeneration, Objects: []ObjectRef{}}
	for _, source := range use.Sources() {
		err := frame.BorrowExport(ctx, use, source, func(_ context.Context, object Object) error {
			grant.Objects = append(grant.Objects, object.Reference)
			return nil
		})
		if err != nil {
			use.BeginClose(err)
			return UseGrant{}, err
		}
	}
	if err := b.checkRequest(validateUseMethod, grant); err != nil {
		use.BeginClose(err)
		return UseGrant{}, err
	}
	instances := make([]string, 0, len(grant.Objects))
	for _, object := range grant.Objects {
		if !slices.Contains(instances, object.Export.InstanceID) {
			instances = append(instances, object.Export.InstanceID)
		}
	}
	if err := b.checkExitCapacity(entry.plan, UseScope, grant.ID, instances); err != nil {
		use.BeginClose(err)
		return UseGrant{}, err
	}
	retained := &browserUse{grant: copyWire(grant), use: use}
	resource, err := use.Lifetime().Own(frame.Resource{Name: useResourcePrefix + use.ID(), Cancel: func(error) { b.fence(entry) }, Close: func(context.Context) (frame.ReleaseState, error) {
		b.mu.Lock()
		released := retained.released()
		b.mu.Unlock()
		if released {
			return frame.ResourceReleased, nil
		}
		b.fence(entry)
		return frame.ResourceUnconfirmed, nil
	}})
	if err != nil {
		use.BeginClose(err)
		return UseGrant{}, err
	}
	retained.resource = resource
	b.mu.Lock()
	if entry.retiring || session.cleanupOnly {
		b.mu.Unlock()
		_ = resource.ConfirmReleased(nil)
		use.BeginClose(frame.ErrFenced)
		return UseGrant{}, frame.ErrFenced
	}
	b.uses[use.ID()] = retained
	copy := copyWire(grant)
	operation.grant = &copy
	b.mu.Unlock()
	return grant, nil
}

func (b *Bridge) ValidateUse(ctx context.Context, token string, grant UseGrant) error {
	session, err := b.liveSession(token)
	if err != nil {
		return err
	}
	b.mu.Lock()
	retained := b.uses[grant.ID]
	valid := retained != nil && retained.grant.Realm == session.realm && sameWire(retained.grant, grant) && !retained.released()
	b.mu.Unlock()
	if !valid {
		return frame.ErrStaleBinding
	}
	if err := retained.use.Context().Err(); err != nil {
		return frame.ErrClosed
	}
	return b.config.Host.ValidateUse(ctx, retained.use)
}

func (b *Bridge) Route(ctx context.Context, token string, grant UseGrant, source, receiver LocalExport) (route RouteGrant, returned error) {
	if err := b.ValidateUse(ctx, token, grant); err != nil {
		return RouteGrant{}, err
	}
	if source.Contract != receiver.Contract || source.Contract.Kind != frame.EventKind || source.Contract.Mode != frame.NativeMode {
		return RouteGrant{}, frame.ErrContractMismatch
	}
	if !grant.grants(source) || !grant.grants(receiver) {
		return RouteGrant{}, frame.ErrStaleBinding
	}
	retained, err := b.reserveRoute(grant.ID)
	if err != nil {
		return RouteGrant{}, err
	}
	committed := false
	defer func() {
		if !committed {
			b.mu.Lock()
			b.routes.cancel()
			b.mu.Unlock()
		}
	}()
	original, err := b.newEventRoute(ctx, retained.use, source, receiver)
	if err != nil {
		return RouteGrant{}, err
	}
	route = RouteGrant{ID: original.ID(), Use: grant, Source: source, Receiver: receiver}
	if err := b.checkRequest(validateRouteMethod, route); err != nil {
		return RouteGrant{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.uses[grant.ID] != retained || retained.released() {
		return RouteGrant{}, frame.ErrClosed
	}
	b.routes.commit(route.ID, browserRoute{grant: copyWire(route), original: original})
	committed = true
	return route, nil
}

// grants reports an object this grant exposes.
func (g UseGrant) grants(export LocalExport) bool {
	return slices.ContainsFunc(g.Objects, func(object ObjectRef) bool { return object.Export == export })
}

func (b *Bridge) reserveRoute(useID string) (*browserUse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	retained := b.uses[useID]
	if retained == nil {
		return nil, frame.ErrClosed
	}
	return retained, b.routes.reserve()
}

func (b *Bridge) newEventRoute(ctx context.Context, use *frame.Use, source, receiver LocalExport) (*frame.EventRoute, error) {
	sourceRef, err := localSource(ctx, use, source)
	if err != nil {
		return nil, err
	}
	receiverRef, err := localSource(ctx, use, receiver)
	if err != nil {
		return nil, err
	}
	return b.config.Host.NewEventRoute(ctx, use, use, sourceRef, receiverRef)
}

func (b *Bridge) ValidateRoute(ctx context.Context, token string, route RouteGrant) error {
	b.mu.Lock()
	original, exists := b.routes.lookup(route.ID)
	b.mu.Unlock()
	if !exists || !sameWire(original.grant, route) {
		return frame.ErrStaleBinding
	}
	if err := b.ValidateUse(ctx, token, route.Use); err != nil {
		return err
	}
	return b.config.Host.ValidateEventRoute(ctx, original.original)
}

func localSource(ctx context.Context, use *frame.Use, local LocalExport) (frame.ExportRef, error) {
	for _, source := range use.Sources() {
		var matches bool
		err := frame.BorrowExport(ctx, use, source, func(_ context.Context, object Object) error { matches = object.Reference.Export == local; return nil })
		if err != nil {
			return frame.ExportRef{}, err
		}
		if matches {
			return source, nil
		}
	}
	return frame.ExportRef{}, frame.ErrContractMismatch
}
