package browser

import (
	"context"
	"errors"
	"slices"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
)

func (b *Bridge) Acquire(ctx context.Context, token string, request AcquireRequest) (grant UseGrant, returned error) {
	session, err := b.session(token, true)
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
	b.mu.Lock()
	old, lookupErr := b.operationLocked(ctx, request.OperationID)
	if lookupErr != nil {
		b.mu.Unlock()
		return UseGrant{}, lookupErr
	}
	if old != nil {
		if old.kind != acquireOperation || old.realm != session.realm || old.digest != digest {
			b.mu.Unlock()
			return UseGrant{}, sqlite.ErrConflict
		}
		done := old.done
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return UseGrant{}, ctx.Err()
		case <-done:
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if old.grant == nil {
			return UseGrant{}, old.failure
		}
		return clone(*old.grant), nil
	}
	if session.cleanupOnly {
		b.mu.Unlock()
		return UseGrant{}, frame.ErrFenced
	}
	entry := b.executions[request.Execution.ID]
	selection := b.snapshots[request.SnapshotID]
	if entry == nil || entry.plan.Execution != request.Execution || entry.plan.Realm != session.realm || entry.retiring || selection == nil || selection.original == nil || selection.realm != session.realm {
		b.mu.Unlock()
		return UseGrant{}, frame.ErrStaleBinding
	}
	if len(b.operations) >= b.config.MaxPending {
		b.mu.Unlock()
		return UseGrant{}, frame.ErrCapacity
	}
	operation := &controlOperation{kind: acquireOperation, realm: session.realm, digest: digest, execution: entry.plan.Execution.ID, parentID: request.ParentUseID, done: make(chan struct{})}
	b.operations[request.OperationID] = operation
	delete(b.snapshots, request.SnapshotID)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		operation.failure = returned
		if operation.grant == nil && operation.failure == nil {
			operation.failure = frame.ErrUnavailable
		}
		b.mu.Unlock()
		returned = errors.Join(returned, b.complete(ctx, request.OperationID, operation))
	}()
	plan, err := selection.bind(request.Selections)
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

func (b *Bridge) useConfig(session *realmSession, entry *browserExecution, parentID string) (frame.UseConfig, error) {
	config := frame.UseConfig{Owner: entry.setup.Lifetime(), Scope: entry.setup.ScopeGrant(), Realm: RealmID(session.realm), ExecutionID: entry.plan.Execution.ID}
	if parentID != "" {
		b.mu.Lock()
		parent := b.uses[parentID]
		valid := parent != nil && parent.grant.Realm == session.realm && parent.grant.Execution == entry.plan.Execution && parent.grant.ScopeID == entry.plan.ScopeID && parent.grant.ScopeGeneration == entry.plan.ScopeGeneration && parent.releaseID == ""
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
	retained := &browserUse{grant: clone(grant), use: use}
	resource, err := use.Lifetime().Own(frame.Resource{Name: "browser use " + use.ID(), Cancel: func(error) { b.fence(entry) }, Close: func(context.Context) (frame.ReleaseState, error) {
		b.mu.Lock()
		released := retained.releaseID != ""
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
	copy := clone(grant)
	operation.grant = &copy
	b.mu.Unlock()
	return grant, nil
}

func (b *Bridge) ValidateUse(ctx context.Context, token string, grant UseGrant) error {
	session, err := b.session(token, true)
	if err != nil {
		return err
	}
	b.mu.Lock()
	retained := b.uses[grant.ID]
	valid := retained != nil && retained.grant.Realm == session.realm && equal(retained.grant, grant) && retained.releaseID == ""
	b.mu.Unlock()
	if !valid {
		return frame.ErrStaleBinding
	}
	if err := retained.use.Context().Err(); err != nil {
		return frame.ErrClosed
	}
	return b.config.Host.ValidateUse(ctx, retained.use)
}

func (b *Bridge) Route(ctx context.Context, token string, grant UseGrant, source, receiver LocalExport) (RouteGrant, error) {
	if err := b.ValidateUse(ctx, token, grant); err != nil {
		return RouteGrant{}, err
	}
	if source.Contract != receiver.Contract || source.Contract.Kind != frame.EventKind || source.Contract.Mode != frame.NativeMode {
		return RouteGrant{}, frame.ErrContractMismatch
	}
	contains := func(export LocalExport) bool {
		for _, object := range grant.Objects {
			if object.Export == export {
				return true
			}
		}
		return false
	}
	if !contains(source) || !contains(receiver) {
		return RouteGrant{}, frame.ErrStaleBinding
	}
	b.mu.Lock()
	retained := b.uses[grant.ID]
	atCapacity := len(b.routes)+b.openingRoutes >= b.config.MaxPending
	if !atCapacity && retained != nil {
		b.openingRoutes++
	}
	b.mu.Unlock()
	if retained == nil {
		return RouteGrant{}, frame.ErrClosed
	}
	if atCapacity {
		return RouteGrant{}, frame.ErrCapacity
	}
	defer func() { b.mu.Lock(); b.openingRoutes--; b.mu.Unlock() }()
	sourceRef, err := localSource(ctx, retained.use, source)
	if err != nil {
		return RouteGrant{}, err
	}
	receiverRef, err := localSource(ctx, retained.use, receiver)
	if err != nil {
		return RouteGrant{}, err
	}
	original, err := b.config.Host.NewEventRoute(ctx, retained.use, retained.use, sourceRef, receiverRef)
	if err != nil {
		return RouteGrant{}, err
	}
	route := RouteGrant{ID: original.ID(), Use: grant, Source: source, Receiver: receiver}
	if err := b.checkRequest(validateRouteMethod, route); err != nil {
		return RouteGrant{}, err
	}
	b.mu.Lock()
	if b.uses[grant.ID] != retained || retained.releaseID != "" {
		b.mu.Unlock()
		return RouteGrant{}, frame.ErrClosed
	}
	b.routes[route.ID] = browserRoute{grant: clone(route), original: original}
	b.mu.Unlock()
	return route, nil
}

func (b *Bridge) ValidateRoute(ctx context.Context, token string, route RouteGrant) error {
	b.mu.Lock()
	original, exists := b.routes[route.ID]
	b.mu.Unlock()
	if !exists || !equal(original.grant, route) {
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

func definitiveRejection(err error) bool {
	if errors.Is(err, sqlite.ErrIndeterminate) || errors.Is(err, frame.ErrUnconfirmedRelease) {
		return false
	}
	return errors.Is(err, frame.ErrInvalid) || errors.Is(err, frame.ErrUnavailable) || errors.Is(err, frame.ErrStaleBinding) || errors.Is(err, frame.ErrContractMismatch) || errors.Is(err, frame.ErrFenced) || errors.Is(err, frame.ErrClosed) || errors.Is(err, frame.ErrCapacity)
}
