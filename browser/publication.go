package browser

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
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
		if session.cleanupOnly || !entry.serves(session.realm, request.Execution) || !entry.hasReady() {
			return framehost.ErrUnavailable
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
	// A metadata decoder may panic or exit this goroutine before producing a
	// result. Only a completed publication can replace this failure.
	returned = fmt.Errorf("%w: publication interrupted", framehost.ErrUnavailable)
	defer func() { returned = b.finish(ctx, request.OperationID, operation, returned) }()
	if err := b.reserveReceipt(ctx, request.OperationID, operation); err != nil {
		return PublicationResult{}, err
	}
	published, err := b.publishObject(ctx, request)
	var problem string
	if err != nil {
		// Host publishes synchronously inside admission. Its errors prove that this
		// command did not publish, even when the transport's context was cancelled.
		problem = classifyProblem(err).message
	}
	b.mu.Lock()
	if problem != "" {
		operation.rejectPublication(problem)
	} else {
		operation.acceptPublication(Publication{ID: published.ID, OperationID: request.OperationID, Revision: strconv.FormatUint(published.Revision, 10), Object: request.Object})
		b.publications[published.ID] = operation
	}
	result, failure := operation.publicationResult()
	b.mu.Unlock()
	return result, failure
}

func (o ObjectRef) membershipOf(unit PlannedUnit) bool {
	return o.validIn(unit.Instance.ID) &&
		o.Export.Contract.Kind == framehost.PointKind && declaresContract(unit, o.Export.Contract)
}

func (b *Bridge) publishObject(ctx context.Context, request PublicationRequest) (framehost.Publication, error) {
	metadata, err := b.config.Host.PrepareMetadata(request.Object.Export.Contract, []byte(request.MetadataJSON))
	if err != nil {
		return framehost.Publication{}, err
	}
	prepared, err := b.config.Host.PreparePublication(ctx, request.Object.Export.InstanceID, framehost.ExportValue{
		Name: request.Object.Export.Name, Contract: request.Object.Export.Contract,
		Metadata: metadata, Value: Object{Reference: request.Object},
	})
	if err != nil {
		return framehost.Publication{}, err
	}
	return b.config.Host.Publish(ctx, request.OperationID, prepared)
}

func (b *Bridge) awaitPublication(ctx context.Context, operation *controlOperation) (PublicationResult, error) {
	if err := operation.wait(ctx); err != nil {
		return PublicationResult{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return operation.publicationResult()
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

func (b *Bridge) Withdraw(ctx context.Context, token string, request Withdrawal) (DecisionResult, error) {
	session, err := b.anySession(token)
	if err != nil {
		return DecisionResult{}, err
	}
	if request.Authority != b.authority || request.PublicationID == "" {
		return DecisionResult{}, frame.ErrInvalid
	}
	b.mu.Lock()
	original := b.publications[request.PublicationID]
	b.mu.Unlock()
	if original == nil {
		return DecisionResult{Decision: Accepted}, nil
	}
	if original.Realm != session.realm {
		return DecisionResult{}, framehost.ErrFenced
	}
	err = b.config.Host.Withdraw(ctx, request.PublicationID)
	if err != nil && !errors.Is(err, framehost.ErrUnavailable) {
		return rejectedDecision(classifyProblem(err).message), nil
	}
	return DecisionResult{Decision: Accepted}, nil
}
