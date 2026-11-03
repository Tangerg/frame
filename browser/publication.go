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
	defer func() {
		b.mu.Lock()
		operation.failure = returned
		b.mu.Unlock()
		returned = errors.Join(returned, b.complete(ctx, request.OperationID, operation))
	}()
	if err := b.reserveReceipt(ctx, request.OperationID, operation); err != nil {
		return PublicationResult{}, err
	}
	published, err := b.publishObject(ctx, request)
	result := PublicationResult{Decision: Accepted, Publication: &Publication{ID: published.ID, OperationID: request.OperationID, Revision: strconv.FormatUint(published.Revision, 10), Object: request.Object}}
	if err != nil {
		// Host publishes synchronously inside admission. Its errors prove that this
		// command did not publish, even when the transport's context was cancelled.
		result = PublicationResult{Decision: Rejected, Problem: classifyProblem(err).message}
	}
	b.mu.Lock()
	operation.publication = clone(result)
	if result.Publication != nil {
		b.publications[result.Publication.ID] = operation
	}
	b.mu.Unlock()
	return result, nil
}

func (b *Bridge) publishObject(ctx context.Context, request PublicationRequest) (frame.Publication, error) {
	metadata, err := b.config.Host.DecodeMetadata(request.Object.Export.Contract, []byte(request.MetadataJSON))
	if err != nil {
		return frame.Publication{}, err
	}
	prepared, err := b.config.Host.PreparePublication(ctx, request.Object.Export.InstanceID, frame.ExportValue{
		Name: request.Object.Export.Name, Contract: request.Object.Export.Contract,
		Metadata: metadata, Value: Object{Reference: request.Object},
	})
	if err != nil {
		return frame.Publication{}, err
	}
	return b.config.Host.Publish(ctx, request.OperationID, prepared)
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

func (b *Bridge) Withdraw(ctx context.Context, token string, request Withdrawal) (PublicationResult, error) {
	session, err := b.session(token, false)
	if err != nil {
		return PublicationResult{}, err
	}
	if request.Authority != b.authority || request.PublicationID == "" {
		return PublicationResult{}, frame.ErrInvalid
	}
	b.mu.Lock()
	original := b.publications[request.PublicationID]
	b.mu.Unlock()
	if original == nil {
		return PublicationResult{Decision: Accepted}, nil
	}
	if original.realm != session.realm {
		return PublicationResult{}, frame.ErrFenced
	}
	err = b.config.Host.Withdraw(ctx, request.PublicationID)
	if err != nil && !errors.Is(err, frame.ErrUnavailable) {
		return PublicationResult{Decision: Rejected, Problem: classifyProblem(err).message}, nil
	}
	return PublicationResult{Decision: Accepted}, nil
}
