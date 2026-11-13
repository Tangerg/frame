package browser

import (
	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

// A completed command owns one business result. Receipt delivery may still be
// unconfirmed, but it cannot replace that result or initiate another command.
type operationResult interface{ controlResult() }

type acquiredUse struct{ grant UseGrant }
type publishedObject struct{ publication Publication }
type publicationRefusal struct{ problem string }
type completedSnapshot struct{}
type failedOperation struct{ cause error }

func (acquiredUse) controlResult()        {}
func (publishedObject) controlResult()    {}
func (publicationRefusal) controlResult() {}
func (completedSnapshot) controlResult()  {}
func (failedOperation) controlResult()    {}

func (o *controlOperation) acceptGrant(grant UseGrant) {
	if o.result != nil || (o.Kind != acquireOperation && o.Kind != acquirePlanOperation) {
		panic("browser: acquisition result already settled or wrong operation kind")
	}
	o.result = acquiredUse{grant: copyWire(grant)}
}

func (o *controlOperation) acceptPublication(publication Publication) {
	if o.result != nil || o.Kind != publishOperation {
		panic("browser: publication result already settled or wrong operation kind")
	}
	o.result = publishedObject{publication: copyWire(publication)}
}

func (o *controlOperation) rejectPublication(problem string) {
	if o.result != nil || o.Kind != publishOperation || problem == "" {
		panic("browser: publication refusal already settled or invalid")
	}
	o.result = publicationRefusal{problem: problem}
}

func (o *controlOperation) conclude(failure error) error {
	if o.result != nil {
		return failure
	}
	if failure == nil {
		switch o.Kind {
		case snapshotOperation, closeSnapshotOperation:
			o.result = completedSnapshot{}
			return nil
		default:
			failure = unknownControlFailure()
		}
	}
	o.result = failedOperation{cause: failure}
	return failure
}

func (o *controlOperation) grantResult() (UseGrant, error) {
	switch result := o.result.(type) {
	case acquiredUse:
		return copyWire(result.grant), nil
	case failedOperation:
		return UseGrant{}, result.cause
	default:
		return UseGrant{}, framehost.ErrUnavailable
	}
}

func (o *controlOperation) publicationResult() (PublicationResult, error) {
	switch result := o.result.(type) {
	case publishedObject:
		publication := copyWire(result.publication)
		return PublicationResult{Decision: Accepted, Publication: &publication}, nil
	case publicationRefusal:
		return PublicationResult{Decision: Rejected, Problem: result.problem}, nil
	case failedOperation:
		return PublicationResult{}, result.cause
	default:
		return PublicationResult{}, framehost.ErrUnavailable
	}
}

func (o *controlOperation) snapshotResult() error {
	switch result := o.result.(type) {
	case failedOperation:
		return result.cause
	case completedSnapshot:
		if o.Kind == closeSnapshotOperation {
			return frame.ErrClosed
		}
		return nil
	default:
		return framehost.ErrUnavailable
	}
}

func (o *controlOperation) receipt() controlReceipt {
	receipt := controlReceipt{controlRequestIdentity: o.controlRequestIdentity}
	switch result := o.result.(type) {
	case acquiredUse:
		grant := copyWire(result.grant)
		grant.ApprovalToken = ""
		receipt.Grant = &grant
	case publishedObject, publicationRefusal:
		publication, _ := o.publicationResult()
		receipt.Publication = &publication
	case failedOperation:
		receipt.Failures = captureReceiptFailures(result.cause)
	}
	return receipt
}

func (r controlReceipt) result(id string, key *realmSigner) (operationResult, error) {
	if !frame.ValidName(id) || !frame.ValidName(r.Realm.ID) || !frame.ValidName(r.Realm.Epoch) {
		return nil, sqlite.ErrConflict
	}
	switch r.Kind {
	case acquireOperation, acquirePlanOperation, publishOperation, snapshotOperation, closeSnapshotOperation:
	default:
		return nil, sqlite.ErrConflict
	}
	if len(r.Failures) != 0 {
		if r.Grant != nil || r.Publication != nil {
			return nil, sqlite.ErrConflict
		}
		failure, err := restoreControlFailure(r.Failures)
		if err != nil {
			return nil, err
		}
		return failedOperation{cause: failure}, nil
	}
	switch r.Kind {
	case acquireOperation, acquirePlanOperation:
		if r.Grant == nil || r.Publication != nil || r.Grant.Realm != r.Realm || r.Grant.OperationID != id || !frame.ValidName(r.Grant.ID) || r.Grant.ApprovalToken != "" {
			return nil, sqlite.ErrConflict
		}
		grant := copyWire(*r.Grant)
		grant.ApprovalToken = key.approval(grant.ID)
		return acquiredUse{grant: grant}, nil
	case publishOperation:
		if r.Publication == nil || r.Grant != nil {
			return nil, sqlite.ErrConflict
		}
		switch r.Publication.Decision {
		case Accepted:
			publication := r.Publication.Publication
			if publication == nil || publication.OperationID != id || !frame.ValidName(publication.ID) || r.Publication.Problem != "" {
				return nil, sqlite.ErrConflict
			}
			return publishedObject{publication: copyWire(*publication)}, nil
		case Rejected:
			if r.Publication.Publication != nil || r.Publication.Problem == "" {
				return nil, sqlite.ErrConflict
			}
			return publicationRefusal{problem: r.Publication.Problem}, nil
		default:
			return nil, sqlite.ErrConflict
		}
	default:
		if r.Grant != nil || r.Publication != nil {
			return nil, sqlite.ErrConflict
		}
		return completedSnapshot{}, nil
	}
}
