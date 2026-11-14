package process

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *Problem) Error() string { return p.Code + ": " + p.Message }
func (p *Problem) Is(target error) bool {
	if p.Code == wire.Errors.Cancelled {
		return target == context.Canceled || target == frame.ErrOutcomeUnknown
	}
	return p.Code == wire.Errors.Unknown && target == frame.ErrOutcomeUnknown
}

func (p *Problem) valid() bool {
	switch p.Code {
	case wire.Errors.Invalid, wire.Errors.Capacity, wire.Errors.Contract,
		wire.Errors.Unknown, wire.Errors.Closed, wire.Errors.Cancelled:
		return true
	default:
		return false
	}
}

type envelope struct {
	ID     string         `json:"id,omitzero"`
	Method string         `json:"method,omitzero"`
	Params jsontext.Value `json:"params,omitzero"`
	Result jsontext.Value `json:"result,omitzero"`
	Error  *Problem       `json:"error,omitzero"`
}

type cancelParams struct {
	ID string `json:"id"`
}
type dispatch func(context.Context, string, jsontext.Value) (jsontext.Value, error)

type peer struct {
	transport io.ReadWriteCloser
	handle    dispatch
	ctx       context.Context
	stop      context.CancelCauseFunc
	done      chan struct{}
	readDone  chan struct{}
	writeDone chan struct{}
	joined    chan struct{}
	out       chan []byte
	mu        sync.Mutex
	maxFrame  int
	next      uint64
	lastIn    uint64
	pending   map[string]chan envelope
	running   map[string]context.CancelFunc
	workers   sync.WaitGroup
	once      sync.Once
}

func newPeer(channel io.ReadWriteCloser, maxFrame int, handle dispatch) *peer {
	ctx, stop := context.WithCancelCause(context.Background())
	callQueueCapacity, replyQueueCapacity := wire.MaxInFlight, wire.MaxInFlight
	// Both call directions share this budget; writeLoop holds its active frame outside the queue.
	return &peer{transport: channel, handle: handle, ctx: ctx, stop: stop, done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}), joined: make(chan struct{}),
		out: make(chan []byte, callQueueCapacity+replyQueueCapacity), maxFrame: maxFrame,
		pending: make(map[string]chan envelope), running: make(map[string]context.CancelFunc)}
}

func (p *peer) start() {
	go p.writeLoop()
	go p.readLoop()
	go func() {
		<-p.readDone
		<-p.writeDone
		p.workers.Wait()
		close(p.joined)
	}()
}

func (p *peer) end(err error) {
	p.once.Do(func() { p.stop(err); _ = p.transport.Close(); close(p.done) })
}

func (p *peer) reduceFrame(size int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if size < 1 || size > p.maxFrame {
		return errors.New("process: invalid negotiated frame limit")
	}
	p.maxFrame = size
	return nil
}

func (p *peer) sendLocked(msg envelope) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(data)+1 > p.maxFrame {
		return &Problem{wire.Errors.Capacity, "frame exceeds limit"}
	}
	if err := p.ctx.Err(); err != nil {
		return context.Cause(p.ctx)
	}
	select {
	case p.out <- append(data, '\n'):
		return nil
	default:
		return &Problem{wire.Errors.Capacity, "output queue full"}
	}
}

func (p *peer) writeLoop() {
	defer close(p.writeDone)
	for {
		select {
		case <-p.ctx.Done():
			return
		case data := <-p.out:
			n, err := p.transport.Write(data)
			if err == nil && n != len(data) {
				err = io.ErrShortWrite
			}
			if err != nil {
				p.end(err)
				return
			}
		}
	}
}

func (p *peer) readLoop() {
	defer close(p.readDone)
	const readSize = 4096
	chunk := make([]byte, readSize)
	var partial []byte
	for {
		n, err := p.transport.Read(chunk)
		for available := chunk[:n]; len(available) > 0; {
			part, rest, complete := bytes.Cut(available, []byte{'\n'})
			p.mu.Lock()
			limit := p.maxFrame
			p.mu.Unlock()
			if len(partial)+len(part)+1 > limit {
				p.end(errors.New("process: incoming frame exceeds limit"))
				return
			}
			partial = append(partial, part...)
			if complete {
				msg, parseErr := parseEnvelope(partial)
				if parseErr == nil {
					parseErr = msg.deliverTo(p)
				}
				if parseErr != nil {
					p.end(parseErr)
					return
				}
				partial = partial[:0]
			}
			available = rest
		}
		if err != nil {
			if len(partial) > 0 && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			p.end(err)
			return
		}
	}
}

var errInvalidEnvelope = errors.New("process: invalid envelope fields")

// incoming is one admitted frame. parseEnvelope settles its variant from the
// members it carries, so delivery never re-derives the kind of message.
type incoming interface {
	deliverTo(*peer) error
}

type (
	incomingRequest struct {
		id     string
		number uint64
		method string
		params jsontext.Value
	}
	incomingCancel struct{ target string }
	incomingReply  struct {
		number uint64
		reply  envelope
	}
)

// parseEnvelope identifies a message by exactly the members it carries; a
// member sent as null is present, which decoding into envelope would hide.
func parseEnvelope(raw []byte) (incoming, error) {
	var fields rawEnvelope
	if err := json.Unmarshal(raw, &fields, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	present := fields.present()
	switch {
	case shapes.Request.Admits(present):
		return fields.request()
	case shapes.Cancel.Admits(present):
		return fields.cancellation()
	case shapes.Result.Admits(present):
		id, number, err := fields.identity()
		return incomingReply{number: number, reply: envelope{ID: id, Result: fields.Result}}, err
	case shapes.Failure.Admits(present):
		return fields.failure()
	default:
		return nil, errInvalidEnvelope
	}
}

func (r rawEnvelope) method() (string, error) {
	var method string
	if r.Method.Kind() != '"' || json.Unmarshal(r.Method, &method) != nil || method == "" || r.Params.Kind() != '{' {
		return "", errInvalidEnvelope
	}
	return method, nil
}

func (r rawEnvelope) request() (incoming, error) {
	method, err := r.method()
	if err != nil || method == wire.Methods.Cancel {
		return nil, errInvalidEnvelope
	}
	id, number, err := r.identity()
	if err != nil {
		return nil, err
	}
	return incomingRequest{id: id, number: number, method: method, params: r.Params}, nil
}

func (r rawEnvelope) cancellation() (incoming, error) {
	method, err := r.method()
	if err != nil || method != wire.Methods.Cancel {
		return nil, errInvalidEnvelope
	}
	var target cancelParams
	if decodeRequest(r.Params, &target) != nil {
		return nil, errors.New("process: invalid cancellation")
	}
	if _, ok := requestNumber(target.ID); !ok {
		return nil, errors.New("process: invalid cancellation identity")
	}
	return incomingCancel{target: target.ID}, nil
}

func (r rawEnvelope) failure() (incoming, error) {
	id, number, err := r.identity()
	if err != nil {
		return nil, err
	}
	var problem Problem
	if r.Error.Kind() != '{' || framevalue.DecodeValue(r.Error, &problem, "error") != nil || !problem.valid() {
		return nil, errInvalidEnvelope
	}
	return incomingReply{number: number, reply: envelope{ID: id, Error: &problem}}, nil
}

func (r rawEnvelope) identity() (string, uint64, error) {
	var id string
	if r.ID.Kind() != '"' || json.Unmarshal(r.ID, &id) != nil {
		return "", 0, errInvalidEnvelope
	}
	number, ok := requestNumber(id)
	if !ok {
		return "", 0, errInvalidEnvelope
	}
	return id, number, nil
}

func requestNumber(id string) (uint64, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	return n, err == nil && n != 0 && strconv.FormatUint(n, 10) == id
}

func (c incomingCancel) deliverTo(p *peer) error {
	p.mu.Lock()
	cancel := p.running[c.target]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (r incomingReply) deliverTo(p *peer) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.number > p.next {
		return errors.New("process: response precedes its request")
	}
	reply := p.pending[r.reply.ID]
	if reply == nil {
		return errors.New("process: duplicate or unknown response")
	}
	delete(p.pending, r.reply.ID)
	reply <- r.reply
	return nil
}

// Request identities must strictly increase.
func (r incomingRequest) deliverTo(p *peer) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.number <= p.lastIn {
		return errors.New("process: invalid request")
	}
	p.lastIn = r.number
	if len(p.running) >= wire.MaxInFlight {
		return p.sendLocked(envelope{ID: r.id, Error: &Problem{wire.Errors.Capacity, "too many active calls"}})
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.running[r.id] = cancel
	p.workers.Add(1)
	go p.respond(ctx, cancel, r)
	return nil
}

func (p *peer) respond(ctx context.Context, cancel context.CancelFunc, request incomingRequest) {
	defer p.workers.Done()
	defer cancel()
	reply := envelope{ID: request.id, Error: unknownProblem()}
	// Handler and diagnostic methods may panic or call Goexit. Only a fully
	// classified reply replaces the default; completion invokes neither again.
	defer func() {
		_ = recover()
		p.mu.Lock()
		delete(p.running, request.id)
		sendErr := p.sendLocked(reply)
		p.mu.Unlock()
		if sendErr != nil {
			p.end(sendErr)
		}
	}()
	result, err := p.handle(ctx, request.method, request.params)
	if err != nil {
		reply = envelope{ID: request.id, Error: responseProblem(err)}
		return
	}
	reply = envelope{ID: request.id, Result: result}
}

func unknownProblem() *Problem {
	return &Problem{wire.Errors.Unknown, "operation did not return a confirmed result"}
}

func responseProblem(err error) *Problem {
	if errors.Is(err, context.Canceled) && !errors.Is(err, errConsumerPanic) {
		return &Problem{wire.Errors.Cancelled, "request cancelled"}
	}
	if errors.Is(err, frame.ErrOutcomeUnknown) {
		return unknownProblem()
	}
	if established, ok := errors.AsType[*Problem](err); ok && established != nil && established.valid() {
		return new(*established)
	}
	return unknownProblem()
}

func decodeRequest(raw jsontext.Value, out any) error {
	if err := framevalue.DecodeValue(raw, out, "wire"); err != nil {
		return &Problem{wire.Errors.Invalid, "invalid payload"}
	}
	return nil
}

func (p *peer) request(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return frame.ContextError(ctx)
	}
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	reply := make(chan envelope, 1)
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return frame.ContextError(ctx)
	}
	if len(p.pending) >= wire.MaxInFlight || p.next == ^uint64(0) {
		p.mu.Unlock()
		return &Problem{wire.Errors.Capacity, "request limit reached"}
	}
	p.next++
	id := strconv.FormatUint(p.next, 10)
	p.pending[id] = reply
	err = p.sendLocked(envelope{ID: id, Method: method, Params: data})
	if err != nil {
		delete(p.pending, id)
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case answer := <-reply:
		return readAnswer(answer, result)
	case <-p.done:
		// A valid reply delivered before EOF still owns this call's result.
		select {
		case answer := <-reply:
			return readAnswer(answer, result)
		default:
		}
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return errors.Join(frame.ErrOutcomeUnknown, context.Cause(p.ctx))
	case <-ctx.Done():
		p.mu.Lock()
		if _, active := p.pending[id]; !active {
			p.mu.Unlock()
			return readAnswer(<-reply, result)
		}
		// Keep the slot until an actual reply or disconnection. Abandoned
		// operations cannot multiply the remote workload beyond our bound.
		data, _ := json.Marshal(cancelParams{id})
		err := p.sendLocked(envelope{Method: wire.Methods.Cancel, Params: data})
		p.mu.Unlock()
		if err != nil {
			p.end(err)
		}
		return errors.Join(frame.ErrOutcomeUnknown, frame.ContextError(ctx))
	}
}

func readAnswer(reply envelope, result any) error {
	if reply.Error != nil {
		return reply.Error
	}
	if err := framevalue.DecodeValue(reply.Result, result, "process reply"); err != nil {
		return fmt.Errorf("%w: invalid reply: %w", frame.ErrOutcomeUnknown, err)
	}
	return nil
}

func (p *peer) acknowledge(ctx context.Context, method string, params any) error {
	var accepted bool
	if err := p.request(ctx, method, params, &accepted); err != nil {
		return err
	}
	if !accepted {
		return errors.New("process: lifecycle operation refused")
	}
	return nil
}
