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
}

// The lease is deliberately per call. Neither overlapping chunks nor chunks
// escaping their original call may advance the acknowledged sequence.
func (d *managedDelivery) receive(wait context.Context, sequence uint64, chunk value.JSON) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("mcp: managed stream consumer panicked")
		}
		if err != nil {
			d.abort(err)
		}
	}()
	if !d.mu.TryLock() {
		return errors.New("mcp: concurrent managed stream delivery")
	}
	defer d.mu.Unlock()
	if err := d.call.Err(); err != nil {
		return context.Cause(d.call)
	}
	if err := wait.Err(); err != nil {
		return context.Cause(wait)
	}
	if d.maxBytes == 0 || sequence == 0 || sequence != d.received+1 {
		return errors.New("mcp: invalid managed stream sequence")
	}
	if int64(len(chunk.String())) > d.maxBytes {
		return frame.ErrCapacity
	}
	if err := d.method.ValidateChunk(chunk.Bytes()); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(d.call)
	stop := context.AfterFunc(wait, cancel)
	defer stop()
	defer cancel()
	if err := d.emit(ctx, chunk); err != nil {
		return err
	}
	d.received = sequence
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
	raw, err = managedRaw(call, s.session, managedInvoke, params)
	err = errors.Join(err, context.Cause(call))
	cancel(nil)
	// A final result raced an unacknowledged chunk. It cannot establish a
	// successful outcome, even when the peer claims completion.
	if !delivery.mu.TryLock() {
		delivery.mu.Lock()
		err = errors.Join(err, errors.New("mcp: managed stream ended with pending delivery"))
	}
	delivery.mu.Unlock()
	return raw, managedDispatchError(method, err)
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
