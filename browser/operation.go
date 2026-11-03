package browser

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

// operationKind names a mutating control operation. Receipts retain it, so
// its values are part of the persisted operation identity.
type operationKind string

const (
	acquireOperation       operationKind = "acquire"
	acquirePlanOperation   operationKind = "acquirePlan"
	snapshotOperation      operationKind = "snapshot"
	closeSnapshotOperation operationKind = "closeSnapshot"
	publishOperation       operationKind = "publish"
)

// controlRequestIdentity is what a repeated operation ID must match: the same
// kind of request, from the same Realm, with the same canonical content.
type controlRequestIdentity struct {
	Kind   operationKind
	Realm  Realm
	Digest value.Digest
}

// controlOperation is one accepted control request. It stays live until its
// receipt is archived; its result fields are final once done is closed.
type controlOperation struct {
	controlRequestIdentity
	execution          string
	parentID           string
	publication        PublicationResult
	grant              *UseGrant
	done               chan struct{}
	failure            error
	receiptReservation *sqlite.ReceiptReservation
}

// errRealmSealed refuses new work in a Realm that has begun cleanup.
var errRealmSealed = fmt.Errorf("%w: realm sealed", frame.ErrFenced)

// begin returns the operation that owns id. A repeated request observes its
// original operation without admission; a new one is admitted by admitLocked,
// which runs with b.mu held, and must be concluded through finish.
func (b *Bridge) begin(ctx context.Context, id string, request controlRequestIdentity, admitLocked func(*controlOperation) error) (operation *controlOperation, replay bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original, err := b.operationLocked(ctx, id)
	if err != nil || original != nil {
		return original, original != nil, err
	}
	if len(b.operations) >= b.config.MaxPending {
		return nil, false, fmt.Errorf("%w: control operations", frame.ErrCapacity)
	}
	operation = &controlOperation{controlRequestIdentity: request, done: make(chan struct{})}
	if err := admitLocked(operation); err != nil {
		return nil, false, err
	}
	b.operations[id] = operation
	return operation, false, nil
}

// repeats accepts a replay only of the exact original request.
func (o *controlOperation) repeats(request controlRequestIdentity) error {
	if o.controlRequestIdentity != request {
		return sqlite.ErrConflict
	}
	return nil
}

// finish concludes a new operation with failure and archives its receipt.
func (b *Bridge) finish(ctx context.Context, id string, operation *controlOperation, failure error) error {
	b.mu.Lock()
	operation.failure = failure
	b.mu.Unlock()
	return errors.Join(failure, b.complete(ctx, id, operation))
}

// grantResult projects an acquisition. An operation that concluded without a
// grant never reads as a successful acquisition.
func (o *controlOperation) grantResult() (UseGrant, error) {
	switch {
	case o.grant != nil:
		return copyWire(*o.grant), nil
	case o.failure != nil:
		return UseGrant{}, o.failure
	default:
		return UseGrant{}, frame.ErrUnavailable
	}
}

type controlReceipt struct {
	controlRequestIdentity
	Publication PublicationResult
	Grant       *UseGrant
	Failures    []string
}

const unknownReceiptFailure = "unknown"

func (o *controlOperation) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return cancellation.Err(ctx)
	case <-o.done:
		return nil
	}
}

// awaitGrant waits for a replayed acquisition and returns its original result.
func (b *Bridge) awaitGrant(ctx context.Context, operation *controlOperation) (UseGrant, error) {
	if err := operation.wait(ctx); err != nil {
		return UseGrant{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return operation.grantResult()
}

// Arbitrary application diagnostics may contain secrets. Replay restores stable
// failure categories, never application error strings or a new side effect.
var receiptFailures = []struct {
	code string
	err  error
}{
	{"invalid", frame.ErrInvalid}, {"closed", frame.ErrClosed}, {"stale", frame.ErrStaleBinding},
	{"unavailable", frame.ErrUnavailable}, {"in_use", frame.ErrInUse}, {"ambiguous", frame.ErrAmbiguous},
	{"dependency_cycle", frame.ErrDependencyCycle}, {"contract", frame.ErrContractMismatch},
	{"recovery", frame.ErrRecoveryRequired}, {"fenced", frame.ErrFenced}, {"capacity", frame.ErrCapacity},
	{"resync", frame.ErrResyncRequired}, {"unconfirmed", frame.ErrUnconfirmedRelease}, {"conflict", sqlite.ErrConflict},
	{"cancelled", context.Canceled}, {"deadline", context.DeadlineExceeded}, {unknownReceiptFailure, sqlite.ErrIndeterminate},
}

func (o *controlOperation) receipt() controlReceipt {
	r := controlReceipt{controlRequestIdentity: o.controlRequestIdentity, Publication: o.publication, Grant: o.grant}
	if o.grant != nil {
		grant := *o.grant
		grant.ApprovalToken = ""
		r.Grant = &grant
	}
	if o.failure != nil {
		for _, f := range receiptFailures {
			if errors.Is(o.failure, f.err) {
				r.Failures = append(r.Failures, f.code)
			}
		}
		if len(r.Failures) == 0 {
			r.Failures = []string{unknownReceiptFailure}
		}
	}
	return r
}

func (r controlReceipt) operation(key *realmSigner) (*controlOperation, error) {
	o := &controlOperation{controlRequestIdentity: r.controlRequestIdentity, publication: r.Publication, grant: r.Grant, done: make(chan struct{})}
	if o.grant != nil {
		o.grant.ApprovalToken = key.approval(o.grant.ID)
	}
	for _, c := range r.Failures {
		var cause error
		for _, f := range receiptFailures {
			if c == f.code {
				cause = f.err
				break
			}
		}
		if cause == nil {
			return nil, sqlite.ErrConflict
		}
		o.failure = errors.Join(o.failure, cause)
	}
	close(o.done)
	return o, nil
}

type receiptRead struct {
	done      chan struct{}
	operation *controlOperation
	err       error
}

// Reserve receipt lookups alongside live operations before entering the single
// SQLite connection. Concurrent observations of the same identity share a read.
func (b *Bridge) operationLocked(ctx context.Context, id string) (*controlOperation, error) {
	if !frame.ValidName(id) {
		return nil, frame.ErrInvalid
	}
	for {
		if pending := b.receiptReads[id]; pending != nil {
			b.mu.Unlock()
			select {
			case <-ctx.Done():
				b.mu.Lock()
				return nil, cancellation.Err(ctx)
			case <-pending.done:
			}
			b.mu.Lock()
			if pending.operation != nil || pending.err != nil {
				return pending.operation, pending.err
			}
			continue
		}
		original := b.operations[id]
		if original != nil {
			select {
			case <-original.done:
			default:
				return original, nil
			}
		} else if len(b.receiptReads)+len(b.operations) >= b.config.MaxPending {
			return nil, frame.ErrCapacity
		}
		pending := &receiptRead{done: make(chan struct{})}
		b.receiptReads[id] = pending
		b.mu.Unlock()
		if original != nil {
			if err := b.archive(ctx, id, original); err != nil {
				pending.err = errors.Join(sqlite.ErrIndeterminate, err)
			} else {
				pending.operation = original
			}
		} else {
			pending.operation, pending.err = b.readReceipt(ctx, id)
		}
		b.mu.Lock()
		delete(b.receiptReads, id)
		close(pending.done)
		return pending.operation, pending.err
	}
}

func (b *Bridge) readReceipt(ctx context.Context, id string) (*controlOperation, error) {
	raw, err := b.config.Store.RealmReceipt(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt controlReceipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		return nil, err
	}
	return receipt.operation(&b.signer)
}

func (b *Bridge) archive(ctx context.Context, id string, o *controlOperation) error {
	data, err := json.Marshal(o.receipt())
	if err != nil {
		return err
	}
	if err = o.receiptReservation.Record(ctx, data); err != nil {
		return err
	}
	b.mu.Lock()
	if b.operations[id] == o {
		delete(b.operations, id)
	}
	b.mu.Unlock()
	return nil
}

func (b *Bridge) complete(ctx context.Context, id string, o *controlOperation) error {
	defer close(o.done)
	if o.receiptReservation == nil {
		b.mu.Lock()
		if b.operations[id] == o {
			delete(b.operations, id)
		}
		b.mu.Unlock()
		return nil
	}
	if err := b.archive(context.WithoutCancel(ctx), id, o); err != nil {
		return errors.Join(sqlite.ErrIndeterminate, err)
	}
	return nil
}

func (b *Bridge) reserveReceipt(ctx context.Context, id string, operation *controlOperation) error {
	receipt := operation.receipt()
	identity, err := requestDigest(receipt.controlRequestIdentity)
	if err != nil {
		return err
	}
	for _, failure := range receiptFailures {
		receipt.Failures = append(receipt.Failures, failure.code)
	}
	maximum, err := encodedSize(receipt)
	if err != nil {
		return err
	}
	switch operation.Kind {
	case publishOperation, acquireOperation, acquirePlanOperation:
		if maximum > math.MaxInt64-b.config.MaxMessageBytes {
			return frame.ErrCapacity
		}
		maximum += b.config.MaxMessageBytes
	}
	operation.receiptReservation, err = b.config.Store.ReserveRealmReceipt(ctx, id, identity, maximum)
	return err
}
