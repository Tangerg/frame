package mcp

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"sync"

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
		d.failLocked(errors.New("mcp: concurrent managed stream delivery"))
	}
	if d.failure != nil {
		err = d.failure
		d.mu.Unlock()
		return err
	}
	d.pending = make(chan struct{})
	maxBytes, previous := d.maxBytes, d.received
	d.mu.Unlock()
	defer func() {
		if recover() != nil {
			err = errors.New("mcp: managed stream consumer panicked")
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
	if err := context.Cause(d.call); err != nil {
		return err
	}
	if wait == nil {
		return frame.ErrInvalid
	}
	if err := context.Cause(wait); err != nil {
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
	stop := context.AfterFunc(wait, func() { cancel(context.Cause(wait)) })
	defer stop()
	defer cancel(nil)
	return errors.Join(d.emit(ctx, chunk), context.Cause(ctx))
}

func (d *managedDelivery) checkBytes(size int) error {
	if int64(size) > d.maxBytes {
		return frame.ErrCapacity
	}
	return nil
}

func (s *ManagedService) stream(ctx context.Context, method *portable.Method, params *managedParams, emit process.Emit) (raw jsontext.Value, returned error) {
	call, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	delivery := &managedDelivery{call: call, abort: cancel, method: method, emit: emit}
	lease, err := reserveManagedLease(s.lifetime)
	if err != nil {
		return nil, err
	}
	grant, err := lease.issue(func() (*ManagedCallback, error) { return issuedGrant(s.openStream(call, s.session, delivery.receive)) })
	if err == nil && grant == nil {
		err = errors.New("mcp: missing managed stream grant")
	}
	defer func() { returned = errors.Join(returned, lease.finish(ctx)) }()
	if err != nil {
		return nil, err
	}
	if err := grant.attach(&params.ParamsBase, managedStreamKey); err != nil {
		return nil, err
	}
	delivery.mu.Lock()
	delivery.maxBytes = grant.MaxResponseBytes
	delivery.mu.Unlock()
	if err := context.Cause(call); err != nil {
		return nil, err
	}
	raw, err = managedRaw(call, s.session, managedInvoke, params)
	if err != nil {
		err = errors.Join(err, context.Cause(call))
	}
	return raw, managedDispatchError(delivery.finish(raw, err))
}

func (d *managedDelivery) finish(raw jsontext.Value, err error) error {
	d.mu.Lock()
	d.closed = true
	pending := d.pending
	if pending != nil {
		d.failLocked(errors.New("mcp: managed stream ended with pending delivery"))
	}
	d.mu.Unlock()
	if pending != nil {
		<-pending
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	err = errors.Join(err, d.failure)
	d.abort(errors.New("mcp: managed stream closed"))
	if err == nil {
		err = d.checkBytes(len(raw))
	}
	return err
}

func (s *ManagedService) Stream(ctx context.Context, member string, input value.JSON, emit process.Emit) (value.JSON, error) {
	if emit == nil {
		return value.JSON{}, frame.ErrInvalid
	}
	return s.invoke(ctx, portable.Query, "", member, input, emit)
}

func (s *ManagedService) StreamCommand(ctx context.Context, id, member string, input value.JSON, emit process.Emit) (value.JSON, error) {
	if !validCommandID(id) || emit == nil {
		return value.JSON{}, frame.ErrInvalid
	}
	return s.invoke(ctx, portable.Mutation, id, member, input, emit)
}
