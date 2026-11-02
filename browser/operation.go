package browser

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type controlReceipt struct {
	Kind        string
	Realm       Realm
	Digest      value.Digest
	Publication PublicationResult
	Grant       *UseGrant
	Failures    []string
}

func (o *controlOperation) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-o.done:
		return nil
	}
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
	{"cancelled", context.Canceled}, {"deadline", context.DeadlineExceeded}, {"unknown", sqlite.ErrIndeterminate},
}

func (o *controlOperation) receipt() controlReceipt {
	r := controlReceipt{Kind: o.kind, Realm: o.realm, Digest: o.digest, Publication: o.publication, Grant: o.grant}
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
			r.Failures = []string{"unknown"}
		}
	}
	return r
}

func (r controlReceipt) operation(key *realmSigner) (*controlOperation, error) {
	o := &controlOperation{kind: r.Kind, realm: r.Realm, digest: r.Digest, publication: r.Publication, grant: r.Grant, done: make(chan struct{})}
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
		if pending := b.receiptReaders[id]; pending != nil {
			b.mu.Unlock()
			select {
			case <-ctx.Done():
				b.mu.Lock()
				return nil, context.Cause(ctx)
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
		} else if len(b.receiptReaders)+len(b.operations) >= b.config.MaxPending {
			return nil, frame.ErrCapacity
		}
		pending := &receiptRead{done: make(chan struct{})}
		b.receiptReaders[id] = pending
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
		delete(b.receiptReaders, id)
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
	if err = b.config.Store.RecordRealmReceipt(ctx, id, data); err != nil {
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
	if err := b.archive(context.WithoutCancel(ctx), id, o); err != nil {
		return errors.Join(sqlite.ErrIndeterminate, err)
	}
	return nil
}
