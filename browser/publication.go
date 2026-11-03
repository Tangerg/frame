package browser

import (
	"context"
	"errors"
	"strconv"

	"github.com/Tangerg/frame"
)

// Publish retains the original operation before asking the sole Host catalog
// owner to publish. Repeating a request observes that operation, never another
// object or another membership.
func (b *Bridge) Publish(ctx context.Context, token string, request PublicationRequest) (out PublicationResult, returned error) {
	if err := b.checkRequest(publishMethod, request); err != nil {
		return PublicationResult{}, err
	}
	session, err := b.liveSession(token)
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
	identity := controlRequestIdentity{Kind: publishOperation, Realm: session.realm, Digest: digest}
	operation, replay, err := b.begin(ctx, request.OperationID, identity, func(operation *controlOperation) error {
		entry := b.executions.get(request.Execution.ID)
		if session.cleanupOnly || !entry.servesLocked(session.realm, request.Execution) || entry.ready == nil {
			return frame.ErrUnavailable
		}
		if !request.Object.membershipOf(entry.plan.Units[0]) {
			return frame.ErrContractMismatch
		}
		if err := b.checkPublicationCapacity(entry.plan, request); err != nil {
			return err
		}
		operation.execution = request.Execution.ID
		return nil
	})
	if err != nil {
		return PublicationResult{}, err
	}
	if replay {
		if err := operation.repeats(identity); err != nil {
			return PublicationResult{}, err
		}
		return b.awaitPublication(ctx, operation)
	}
	defer func() { returned = b.finish(ctx, request.OperationID, operation, returned) }()
	if err := b.reserveReceipt(ctx, request.OperationID, operation); err != nil {
		return PublicationResult{}, err
	}
	result := PublicationResult{Decision: Accepted}
	published, err := b.publishObject(ctx, request)
	if err != nil {
		// Host publishes synchronously inside admission. Its errors prove that this
		// command did not publish, even when the transport's context was cancelled.
		result = rejected(classifyProblem(err).message)
	} else {
		result.Publication = &Publication{ID: published.ID, OperationID: request.OperationID, Revision: strconv.FormatUint(published.Revision, 10), Object: request.Object}
	}
	b.mu.Lock()
	operation.publication = copyWire(result)
	if result.Publication != nil {
		b.publications[result.Publication.ID] = operation
	}
	b.mu.Unlock()
	return result, nil
}

// membershipOf reports a dynamic member of a Point that unit declared.
func (o ObjectRef) membershipOf(unit PlannedUnit) bool {
	return o.ObjectID != "" && o.Export.InstanceID == unit.Instance.ID && o.Export.Name != "" &&
		o.Export.Contract.Kind == frame.PointKind && declaresContract(unit, o.Export.Contract)
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

func (b *Bridge) awaitPublication(ctx context.Context, operation *controlOperation) (PublicationResult, error) {
	if err := operation.wait(ctx); err != nil {
		return PublicationResult{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return copyWire(operation.publication), operation.failure
}

func (b *Bridge) Publication(ctx context.Context, token, operationID string) (PublicationResult, error) {
	session, err := b.anySession(token)
	if err != nil {
		return PublicationResult{}, err
	}
	b.mu.Lock()
	operation, err := b.operationLocked(ctx, operationID)
	if err != nil {
		b.mu.Unlock()
		return PublicationResult{}, err
	}
	if operation == nil || operation.Kind != publishOperation || operation.Realm != session.realm {
		b.mu.Unlock()
		return PublicationResult{Decision: Unknown}, nil
	}
	b.mu.Unlock()
	return b.awaitPublication(ctx, operation)
}

func (b *Bridge) Withdraw(ctx context.Context, token string, request Withdrawal) (PublicationResult, error) {
	session, err := b.anySession(token)
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
	if original.Realm != session.realm {
		return PublicationResult{}, frame.ErrFenced
	}
	err = b.config.Host.Withdraw(ctx, request.PublicationID)
	if err != nil && !errors.Is(err, frame.ErrUnavailable) {
		return rejected(classifyProblem(err).message), nil
	}
	return PublicationResult{Decision: Accepted}, nil
}
