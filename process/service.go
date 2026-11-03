package process

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

// Service is one already bound export. Only the member names in that export's
// published contract can be invoked; equal member names across exports are independent.
type Service interface {
	Invoke(context.Context, portable.Invocation) (value.JSON, error)
}
type Handler func(context.Context, portable.Invocation) (value.JSON, error)

func (f Handler) Invoke(ctx context.Context, invocation portable.Invocation) (value.JSON, error) {
	return f(ctx, invocation)
}

type Emit func(context.Context, value.JSON) error
type StreamingService interface {
	Service
	Stream(context.Context, portable.Invocation, Emit) (value.JSON, error)
}

type callParams struct {
	Handle    string         `json:"handle"`
	Member    string         `json:"member"`
	Input     value.JSON     `json:"input"`
	CommandID jsontext.Value `json:"commandID,omitzero"`
	Stream    string         `json:"stream,omitzero"`
}

func (p callParams) invocation() (portable.Invocation, error) {
	if len(p.CommandID) != 0 {
		var id string
		if p.CommandID.Kind() != '"' || json.Unmarshal(p.CommandID, &id) != nil {
			return portable.Invocation{}, mismatch()
		}
		return portable.NewCommandInvocation(id, p.Member, p.Input)
	}
	return portable.NewQueryInvocation(p.Member, p.Input)
}

type chunkParams struct {
	Stream string     `json:"stream"`
	Value  value.JSON `json:"value"`
}
type binding struct {
	contract *portable.Capability
	service  Service
}

func mismatch() error { return &Problem{profile.Errors.Contract, "service contract mismatch"} }

type deliveries struct {
	mu    sync.Mutex
	sinks map[string]*sink
}
type sink struct {
	call   *streamCall
	method *portable.Method
	emit   Emit
}

func (d *deliveries) open(call *streamCall, m *portable.Method, emit Emit) (string, func()) {
	token := rand.Text()
	s := &sink{call: call, method: m, emit: emit}
	d.mu.Lock()
	if d.sinks == nil {
		d.sinks = make(map[string]*sink)
	}
	d.sinks[token] = s
	d.mu.Unlock()
	return token, func() {
		d.mu.Lock()
		delete(d.sinks, token)
		d.mu.Unlock()
	}
}
func (d *deliveries) receive(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
	var part chunkParams
	if err := decode(raw, &part); err != nil {
		return nil, err
	}
	d.mu.Lock()
	s := d.sinks[part.Stream]
	d.mu.Unlock()
	if s == nil {
		return nil, mismatch()
	}
	if err := s.call.deliver(ctx, func(delivery context.Context) error {
		if err := s.method.ValidateChunk(part.Value.Bytes()); err != nil {
			return errors.Join(mismatch(), err)
		}
		return s.emit(delivery, part.Value)
	}); err != nil {
		return nil, err
	}
	return jsontext.Value("true"), nil
}

type remote struct {
	peer       *peer
	handle     string
	contract   *portable.Capability
	deliveries *deliveries
}

func (r *remote) Invoke(ctx context.Context, invocation portable.Invocation) (value.JSON, error) {
	return r.invoke(ctx, invocation, nil)
}
func (r *remote) Stream(ctx context.Context, invocation portable.Invocation, emit Emit) (value.JSON, error) {
	if emit == nil {
		return value.JSON{}, frame.ErrInvalid
	}
	return r.invoke(ctx, invocation, emit)
}
func (r *remote) invoke(ctx context.Context, invocation portable.Invocation, emit Emit) (result value.JSON, err error) {
	method, ok := r.contract.Method(invocation.Member())
	if !ok || (len(method.Descriptor().Stream) > 0) != (emit != nil) {
		return value.JSON{}, mismatch()
	}
	if err := method.ValidateInvocation(invocation); err != nil {
		return value.JSON{}, errors.Join(frame.ErrContractMismatch, err)
	}
	params := callParams{Handle: r.handle, Member: invocation.Member(), Input: invocation.Input()}
	if invocation.Kind() == portable.Command {
		params.CommandID, err = json.Marshal(invocation.CommandID())
		if err != nil {
			return value.JSON{}, err
		}
	}
	if emit != nil {
		call := newStreamCall(ctx)
		token, closeSink := r.deliveries.open(call, method, emit)
		defer func() {
			closeSink()
			err = call.finish(err)
			if err != nil {
				result = value.JSON{}
			}
		}()
		params.Stream = token
		ctx = call.ctx
	}
	if err := r.peer.request(ctx, profile.Methods.Call, params, &result); err != nil {
		return result, err
	}
	if err := method.ValidateOutput(result.Bytes()); err != nil {
		return value.JSON{}, errors.Join(ErrOutcomeUnknown, err)
	}
	return result, nil
}

func dispatchCall(ctx context.Context, conn *peer, services map[string]binding, raw jsontext.Value) (jsontext.Value, error) {
	var params callParams
	if err := decode(raw, &params); err != nil {
		return nil, err
	}
	selected, ok := services[params.Handle]
	if !ok {
		return nil, mismatch()
	}
	method, ok := selected.contract.Method(params.Member)
	invocation, err := params.invocation()
	if err != nil || !ok || method.ValidateInvocation(invocation) != nil || (len(method.Descriptor().Stream) > 0) != (params.Stream != "") {
		return nil, mismatch()
	}
	var result value.JSON
	if params.Stream != "" {
		implementation, ok := selected.service.(StreamingService)
		if !ok {
			return nil, mismatch()
		}
		result, err = sendStream(ctx, conn, implementation, method, params, invocation)
	} else {
		result, err = selected.service.Invoke(ctx, invocation)
	}
	if err != nil {
		// Handler errors cannot prove that an admitted effect did not occur.
		// Only the business owner can reconcile an absent result.
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if err := method.ValidateOutput(result.Bytes()); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	return result.Bytes(), nil
}

func sendStream(ctx context.Context, conn *peer, source StreamingService, method *portable.Method, params callParams, invocation portable.Invocation) (result value.JSON, err error) {
	call := newStreamCall(ctx)
	defer func() { err = call.finish(err) }()
	return source.Stream(call.ctx, invocation, func(wait context.Context, chunk value.JSON) error {
		return call.deliver(wait, func(delivery context.Context) error {
			if err := method.ValidateChunk(chunk.Bytes()); err != nil {
				return err
			}
			return conn.acknowledge(delivery, profile.Methods.Chunk, chunkParams{params.Stream, chunk})
		})
	})
}
