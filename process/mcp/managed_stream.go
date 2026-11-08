package mcp

import (
	"context"
	"errors"
	"sync"

	"github.com/Tangerg/frame/internal/cancellation"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

const managedStreamKey = managedNamespace + "stream.v1"

// OpenManagedStream grants only Qimu's acknowledged qimu.stream/deliver.v1
// callback for this invocation. The host authenticates the route and returns
// true to the backend only when receive returns nil.
type OpenManagedStream func(ctx context.Context, session *sdk.ClientSession, receive func(context.Context, uint64, value.JSON) error) (ManagedCallback, error)

type managedDelivery struct {
	mu       sync.Mutex
	abort    context.CancelCauseFunc
	call     context.Context
	method   *portable.Method
	emit     process.Emit
	maxBytes int64
	received uint64
	pending  chan struct{}
	closed   bool
	failure  error
}

// The lease is deliberately per call. Neither overlapping chunks nor chunks
// escaping their original call may advance the acknowledged sequence.
func (d *managedDelivery) failLocked(cause error) error {
	if d.failure == nil {
		d.failure = cause
		d.abort(cause)
	}
	return d.failure
}

func (d *managedDelivery) receive(wait context.Context, sequence uint64, chunk value.JSON) (err error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return errors.New("mcp: managed stream is closed")
	}
	if d.pending != nil {
		_ = d.failLocked(errors.New("mcp: concurrent managed stream delivery"))
	}
	if d.failure != nil {
		err = d.failure
		d.mu.Unlock()
		return err
	}
	d.pending = make(chan struct{})
	maxBytes, previous := d.maxBytes, d.received
	d.mu.Unlock()
	returned := false
	defer func() {
		if recovered := recover(); recovered != nil {
			err = newCallbackPanic("mcp: managed stream consumer panicked", recovered)
		} else if !returned && err == nil {
			err = errors.New("mcp: managed stream consumer exited without returning")
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if err != nil {
			err = d.failLocked(err)
		} else {
			d.received = sequence
		}
		close(d.pending)
		d.pending = nil
	}()
	if err := cancellation.Err(d.call); err != nil {
		return err
	}
	if wait == nil {
		return frame.ErrInvalid
	}
	if err := cancellation.Err(wait); err != nil {
		return err
	}
	if maxBytes == 0 || sequence == 0 || sequence != previous+1 {
		return errors.New("mcp: invalid managed stream sequence")
	}
	if int64(len(chunk.String())) > maxBytes {
		return frame.ErrCapacity
	}
	if err := d.method.ValidateChunk(chunk.Bytes()); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(d.call)
	stop := context.AfterFunc(wait, func() { cancel(cancellation.Err(wait)) })
	defer stop()
	defer cancel(nil)
	err = d.emit(ctx, chunk)
	returned = true
	return errors.Join(err, cancellation.Err(ctx))
}

func (d *managedDelivery) checkBytes(size int) error {
	if int64(size) > d.maxBytes {
		return frame.ErrCapacity
	}
	return nil
}

func (s *ManagedService) stream(ctx context.Context, method *portable.Method, params *managedParams, emit process.Emit) (reply managedReply, returned error) {
	call, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	delivery := &managedDelivery{call: call, abort: cancel, method: method, emit: emit}
	lease, err := reserveManagedLease(s.lifetime)
	if err != nil {
		return managedReply{}, err
	}
	grant, err := lease.issue(func() (*ManagedCallback, error) { return issuedGrant(s.openStream(call, s.session, delivery.receive)) })
	if err == nil && grant == nil {
		err = errors.New("mcp: missing managed stream grant")
	}
	defer func() { returned = errors.Join(returned, lease.finish(ctx)) }()
	if err != nil {
		return managedReply{}, err
	}
	if err := grant.attach(&params.ParamsBase, managedStreamKey); err != nil {
		return managedReply{}, err
	}
	delivery.mu.Lock()
	delivery.maxBytes = grant.MaxResponseBytes
	delivery.mu.Unlock()
	if err := cancellation.Err(call); err != nil {
		return managedReply{}, err
	}
	reply, err = requestManaged(call, s.session, managedInvoke, params)
	deliveryErr := delivery.finish()
	if err != nil {
		return managedReply{}, errors.Join(process.ErrOutcomeUnknown, err, deliveryErr)
	}
	if reply.refusal != nil {
		return reply, deliveryErr
	}
	if deliveryErr != nil {
		return managedReply{}, errors.Join(process.ErrOutcomeUnknown, deliveryErr)
	}
	if err := delivery.checkBytes(len(reply.value)); err != nil {
		return managedReply{}, errors.Join(process.ErrOutcomeUnknown, err)
	}
	return reply, nil
}

func (d *managedDelivery) finish() error {
	d.mu.Lock()
	d.closed = true
	pending := d.pending
	if pending != nil {
		_ = d.failLocked(errors.New("mcp: managed stream ended with pending delivery"))
	}
	d.mu.Unlock()
	if pending != nil {
		<-pending
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.abort(errors.New("mcp: managed stream closed"))
	return d.failure
}

func (s *ManagedService) Stream(ctx context.Context, invocation portable.Invocation, emit process.Emit) (value.JSON, error) {
	if emit == nil {
		return value.JSON{}, frame.ErrInvalid
	}
	return s.invoke(ctx, invocation, emit)
}
