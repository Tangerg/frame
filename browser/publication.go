package browser

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
)

// Publish retains the original operation before asking the sole Host catalog
// owner to publish. Repeating a request observes that operation, never another
// object or another membership.
func (b *Bridge) Publish(ctx context.Context, token string, request PublicationRequest) (out PublicationResult, returned error) {
	if err := b.checkRequest(publishOperation, request); err != nil {
		return PublicationResult{}, err
	}
	session, err := b.session(token, true)
	if err != nil {
		return PublicationResult{}, err
	}
	if !frame.ValidName(request.OperationID) || request.Authority != b.authority || request.Realm != session.realm {
		return PublicationResult{}, frame.ErrInvalid
	}
	digest, err := requestDigest(request)
	if err != nil {
		return PublicationResult{}, err
	}
	b.mu.Lock()
	prior, lookupErr := b.operationLocked(ctx, request.OperationID)
	if lookupErr != nil {
		b.mu.Unlock()
		return PublicationResult{}, lookupErr
	}
	if prior != nil {
		if prior.kind != publishOperation || prior.realm != session.realm || prior.digest != digest {
			b.mu.Unlock()
			return PublicationResult{}, sqlite.ErrConflict
		}
		b.mu.Unlock()
		return b.waitPublication(ctx, prior)
	}
	entry := b.executions[request.Execution.ID]
	if session.cleanupOnly || entry == nil || entry.retiring || entry.plan.Execution != request.Execution || entry.plan.Realm != session.realm || entry.readyOperation == "" {
		b.mu.Unlock()
		return PublicationResult{}, frame.ErrUnavailable
	}
	if len(b.operations) >= b.config.MaxPending {
		b.mu.Unlock()
		return PublicationResult{}, frame.ErrCapacity
	}
	unit := entry.plan.Units[0]
	if request.Object.ObjectID == "" || request.Object.Export.InstanceID != unit.Instance.ID || request.Object.Export.Name == "" || request.Object.Export.Contract.Kind != frame.PointKind || !slices.ContainsFunc(unit.Exports, func(declaration frame.ExportDeclaration) bool {
		return declaration.Contract == request.Object.Export.Contract
	}) {
		b.mu.Unlock()
		return PublicationResult{}, frame.ErrContractMismatch
	}
	if err := b.checkPublicationCapacity(entry.plan, request); err != nil {
		b.mu.Unlock()
		return PublicationResult{}, err
	}
	operation := &controlOperation{kind: publishOperation, realm: session.realm, digest: digest, execution: request.Execution.ID, done: make(chan struct{})}
	b.operations[request.OperationID] = operation
	b.mu.Unlock()
	defer func() { returned = errors.Join(returned, b.complete(ctx, request.OperationID, operation)) }()
	metadata, err := b.config.Host.DecodeMetadata(request.Object.Export.Contract, []byte(request.MetadataJSON))
	var result PublicationResult
	if err == nil {
		var published frame.Publication
		published, err = b.config.Host.Publish(ctx, unit.Instance.ID, request.OperationID, frame.ExportValue{Name: request.Object.Export.Name, Contract: request.Object.Export.Contract, Metadata: metadata, Value: Object{Reference: request.Object}})
		if err == nil {
			result = PublicationResult{Decision: Accepted, Publication: &Publication{ID: published.ID, OperationID: request.OperationID, Revision: strconv.FormatUint(published.Revision, 10), Object: request.Object}}
		}
	}
	if err != nil {
		if definitiveRejection(err) {
			result = PublicationResult{Decision: Rejected, Problem: classifyProblem(err).message}
			err = nil
		} else {
			result = PublicationResult{Decision: Unknown}
		}
	}
	b.mu.Lock()
	operation.publication = clone(result)
	if result.Publication != nil {
		b.publications[result.Publication.ID] = operation
	}
	operation.failure = err
	b.mu.Unlock()
	return result, err
}

func (b *Bridge) waitPublication(ctx context.Context, operation *controlOperation) (PublicationResult, error) {
	select {
	case <-ctx.Done():
		return PublicationResult{}, ctx.Err()
	case <-operation.done:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return clone(operation.publication), operation.failure
}

func (b *Bridge) Publication(ctx context.Context, token, operationID string) (PublicationResult, error) {
	session, err := b.session(token, false)
	if err != nil {
		return PublicationResult{}, err
	}
	b.mu.Lock()
	operation, err := b.operationLocked(ctx, operationID)
	if err != nil {
		b.mu.Unlock()
		return PublicationResult{}, err
	}
	if operation == nil || operation.kind != publishOperation || operation.realm != session.realm {
		b.mu.Unlock()
		return PublicationResult{Decision: Unknown}, nil
	}
	b.mu.Unlock()
	return b.waitPublication(ctx, operation)
}

func (b *Bridge) Withdraw(ctx context.Context, token string, request Withdrawal) (out PublicationResult, returned error) {
	session, err := b.session(token, false)
	if err != nil {
		return PublicationResult{}, err
	}
	if !frame.ValidName(request.OperationID) || request.Authority != b.authority || request.PublicationID == "" {
		return PublicationResult{}, frame.ErrInvalid
	}
	digest, err := requestDigest(request)
	if err != nil {
		return PublicationResult{}, err
	}
	b.mu.Lock()
	prior, lookupErr := b.operationLocked(ctx, request.OperationID)
	if lookupErr != nil {
		b.mu.Unlock()
		return PublicationResult{}, lookupErr
	}
	if prior != nil {
		if prior.kind != withdrawOperation || prior.realm != session.realm || prior.digest != digest {
			b.mu.Unlock()
			return PublicationResult{}, sqlite.ErrConflict
		}
		b.mu.Unlock()
		return b.waitPublication(ctx, prior)
	}
	original := b.publications[request.PublicationID]
	if original == nil || original.realm != session.realm {
		b.mu.Unlock()
		return PublicationResult{Decision: Unknown}, nil
	}
	if len(b.operations) >= b.config.MaxPending {
		b.mu.Unlock()
		return PublicationResult{}, frame.ErrCapacity
	}
	operation := &controlOperation{kind: withdrawOperation, realm: session.realm, digest: digest, execution: original.execution, done: make(chan struct{})}
	b.operations[request.OperationID] = operation
	alreadyWithdrawn := original.withdrawn
	b.mu.Unlock()
	defer func() { returned = errors.Join(returned, b.complete(ctx, request.OperationID, operation)) }()
	if !alreadyWithdrawn {
		err = b.config.Host.Withdraw(ctx, request.PublicationID)
	}
	// An unavailable original publication is already sealed by its Host owner.
	if errors.Is(err, frame.ErrUnavailable) {
		err = nil
	}
	result := PublicationResult{Decision: Accepted}
	if err != nil {
		result.Decision = Unknown
	}
	b.mu.Lock()
	if err == nil {
		original.withdrawn = true
	}
	operation.publication = result
	operation.failure = err
	b.mu.Unlock()
	return result, err
}
