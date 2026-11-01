package process

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"errors"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/jsonvalue"
	"github.com/Tangerg/frame/portable"
)

// Service is one already bound export. Only the member names in that export's
// published contract can be invoked; equal member names across exports are independent.
type Service interface {
	Call(context.Context, string, jsonvalue.Value) (jsonvalue.Value, error)
}
type Handler func(context.Context, string, jsonvalue.Value) (jsonvalue.Value, error)

func (f Handler) Call(ctx context.Context, name string, input jsonvalue.Value) (jsonvalue.Value, error) {
	return f(ctx, name, input)
}

type Emit func(context.Context, jsonvalue.Value) error
type StreamingService interface {
	Service
	Stream(context.Context, string, jsonvalue.Value, Emit) (jsonvalue.Value, error)
}

type callParams struct {
	Handle string          `json:"handle"`
	Member string          `json:"member"`
	Input  jsonvalue.Value `json:"input"`
	Stream string          `json:"stream,omitzero"`
}
type chunkParams struct {
	Stream string          `json:"stream"`
	Value  jsonvalue.Value `json:"value"`
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
	mu     sync.Mutex
	ctx    context.Context
	method *portable.Method
	emit   Emit
	closed bool
}

func (d *deliveries) open(ctx context.Context, m *portable.Method, emit Emit) (string, func()) {
	token := rand.Text()
	s := &sink{ctx: ctx, method: m, emit: emit}
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
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
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
	if s == nil || !s.mu.TryLock() {
		return nil, mismatch()
	}
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil {
		return nil, context.Canceled
	}
	if err := s.method.ValidateChunk(part.Value.JSON()); err != nil {
		return nil, mismatch()
	}
	child, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	if err := s.emit(child, part.Value); err != nil {
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

func (r *remote) Call(ctx context.Context, member string, input jsonvalue.Value) (jsonvalue.Value, error) {
	return r.invoke(ctx, member, input, nil)
}
func (r *remote) Stream(ctx context.Context, member string, input jsonvalue.Value, emit Emit) (jsonvalue.Value, error) {
	if emit == nil {
		return jsonvalue.Value{}, frame.ErrInvalid
	}
	return r.invoke(ctx, member, input, emit)
}
func (r *remote) invoke(ctx context.Context, member string, input jsonvalue.Value, emit Emit) (jsonvalue.Value, error) {
	method, ok := r.contract.Method(member)
	if !ok || (len(method.Descriptor().Stream) > 0) != (emit != nil) {
		return jsonvalue.Value{}, mismatch()
	}
	if err := method.ValidateInput(input); err != nil {
		return jsonvalue.Value{}, errors.Join(frame.ErrContractMismatch, err)
	}
	params := callParams{Handle: r.handle, Member: member, Input: input}
	if emit != nil {
		streamCtx, cancel := context.WithCancel(ctx)
		token, closeSink := r.deliveries.open(streamCtx, method, emit)
		defer func() { cancel(); closeSink() }()
		params.Stream = token
	}
	var result jsonvalue.Value
	if err := r.peer.request(ctx, profile.Methods.Call, params, &result); err != nil {
		return result, err
	}
	if err := method.ValidateOutput(result.JSON()); err != nil {
		return jsonvalue.Value{}, errors.Join(ErrOutcomeUnknown, err)
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
	if !ok || method.ValidateInput(params.Input) != nil || (len(method.Descriptor().Stream) > 0) != (params.Stream != "") {
		return nil, mismatch()
	}
	var result jsonvalue.Value
	var err error
	if params.Stream != "" {
		implementation, ok := selected.service.(StreamingService)
		if !ok {
			return nil, mismatch()
		}
		result, err = sendStream(ctx, conn, implementation, method, params)
	} else {
		result, err = selected.service.Call(ctx, params.Member, params.Input)
	}
	if err != nil {
		return nil, err
	}
	if err := method.ValidateOutput(result.JSON()); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	return result.JSON(), nil
}

// Concurrent or escaped emitters cannot publish a chunk after the final result.
// Both the caller's cancellation and the emitter's shorter wait bound apply.
func sendStream(ctx context.Context, conn *peer, source StreamingService, method *portable.Method, params callParams) (jsonvalue.Value, error) {
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	var guard sync.Mutex
	sealed := false
	result, err := source.Stream(lifetime, params.Member, params.Input, func(wait context.Context, chunk jsonvalue.Value) error {
		if !guard.TryLock() {
			return &Problem{profile.Errors.Invalid, "concurrent chunk delivery"}
		}
		defer guard.Unlock()
		if wait == nil || sealed || lifetime.Err() != nil {
			return &Problem{profile.Errors.Closed, "stream closed"}
		}
		if err := wait.Err(); err != nil {
			return context.Cause(wait)
		}
		if err := method.ValidateChunk(chunk.JSON()); err != nil {
			return err
		}
		child, stop := context.WithCancel(lifetime)
		release := context.AfterFunc(wait, stop)
		defer release()
		defer stop()
		return conn.acknowledge(child, profile.Methods.Chunk, chunkParams{params.Stream, chunk})
	})
	if !guard.TryLock() {
		cancel()
		guard.Lock()
		err = errors.Join(err, ErrOutcomeUnknown)
	}
	sealed = true
	guard.Unlock()
	return result, err
}
