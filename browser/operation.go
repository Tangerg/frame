package browser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
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

// The immutable business result and the original receipt reservation have
// separate owners. done joins the first archive attempt, including uncertainty.
type controlOperation struct {
	controlRequestIdentity
	execution          string
	parentID           string
	result             operationResult
	done               chan struct{}
	receiptReservation *sqlite.ReceiptReservation
}

var errRealmSealed = fmt.Errorf("%w: realm sealed", framehost.ErrFenced)

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

func (o *controlOperation) repeats(request controlRequestIdentity) error {
	if o.controlRequestIdentity != request {
		return sqlite.ErrConflict
	}
	return nil
}

func (b *Bridge) finish(ctx context.Context, id string, operation *controlOperation, failure error) error {
	failure = captureControlFailure(failure)
	b.mu.Lock()
	failure = operation.conclude(failure)
	b.mu.Unlock()
	defer close(operation.done)
	if operation.receiptReservation == nil {
		b.mu.Lock()
		if b.operations[id] == operation {
			delete(b.operations, id)
		}
		b.mu.Unlock()
		return failure
	}
	if err := b.archive(context.WithoutCancel(ctx), id, operation); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return errors.Join(failure, sqlite.ErrIndeterminate, err)
	}
	return failure
}

type controlReceipt struct {
	controlRequestIdentity
	Publication *PublicationResult `json:"publication,omitempty"`
	Grant       *UseGrant          `json:"grant,omitempty"`
	Failures    []string           `json:"failures"`
}

func (o *controlOperation) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return frame.ContextError(ctx)
	case <-o.done:
		return nil
	}
}

func (b *Bridge) awaitGrant(ctx context.Context, operation *controlOperation) (UseGrant, error) {
	if err := operation.wait(ctx); err != nil {
		return UseGrant{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return operation.grantResult()
}

func (r controlReceipt) operation(id string, key *realmSigner) (*controlOperation, error) {
	result, err := r.result(id, key)
	if err != nil {
		return nil, err
	}
	o := &controlOperation{controlRequestIdentity: r.controlRequestIdentity, result: result, done: make(chan struct{})}
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
	if value.IsNil(ctx) || !frame.ValidName(id) {
		return nil, frame.ErrInvalid
	}
	for {
		if pending := b.receiptReads[id]; pending != nil {
			b.mu.Unlock()
			select {
			case <-ctx.Done():
				b.mu.Lock()
				return nil, frame.ContextError(ctx)
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
			switch err := b.archive(ctx, id, original); {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				pending.err = errors.Join(sqlite.ErrIndeterminate, err)
			default:
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
	if err = value.DecodeValue(raw, &receipt, "control receipt"); err != nil {
		return nil, errors.Join(sqlite.ErrConflict, err)
	}
	return receipt.operation(id, &b.signer)
}

func (b *Bridge) archive(ctx context.Context, id string, o *controlOperation) error {
	data, err := marshalControl(o.receipt())
	if err != nil {
		return err
	}
	err = o.receiptReservation.Record(ctx, data)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Only the reservation owner can prove that an unacknowledged reservation
	// rolled back. An unavailable settlement must retain this original operation.
	b.mu.Lock()
	if b.operations[id] == o {
		delete(b.operations, id)
	}
	b.mu.Unlock()
	return err
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
