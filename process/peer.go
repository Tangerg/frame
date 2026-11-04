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

	"github.com/Tangerg/frame/internal/cancellation"
	framevalue "github.com/Tangerg/frame/value"
)

// A dispatched request without a validated result remains unknown, even when
// its handler confirms cancellation. Cancellation cannot settle an effect; only
// the business owner can establish its original outcome or authorize a retry.
var ErrOutcomeUnknown = errors.New("process: outcome unknown")

type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *Problem) Error() string { return p.Code + ": " + p.Message }
func (p *Problem) Is(target error) bool {
	if p.Code == wire.Errors.Cancelled {
		return target == context.Canceled || target == ErrOutcomeUnknown
	}
	return p.Code == wire.Errors.Unknown && target == ErrOutcomeUnknown
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
					parseErr = p.receive(msg)
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

// parseEnvelope identifies a message by exactly the members it carries; a
// member sent as null is present, which decoding into envelope would hide.
func parseEnvelope(raw []byte) (envelope, error) {
	var fields rawEnvelope
	if err := json.Unmarshal(raw, &fields, json.RejectUnknownMembers(true)); err != nil {
		return envelope{}, err
	}
	present := fields.present()
	switch {
	case shapes.Request.Admits(present):
		return fields.request(false)
	case shapes.Cancel.Admits(present):
		return fields.request(true)
	case shapes.Result.Admits(present):
		id, err := fields.identity()
		return envelope{ID: id, Result: fields.Result}, err
	case shapes.Failure.Admits(present):
		return fields.failure()
	default:
		return envelope{}, errInvalidEnvelope
	}
}

// request admits a call, which carries its identity, or a cancellation,
// which carries none.
func (r rawEnvelope) request(cancel bool) (envelope, error) {
	var msg envelope
	if r.Method.Kind() != '"' || json.Unmarshal(r.Method, &msg.Method) != nil || msg.Method == "" || r.Params.Kind() != '{' {
		return envelope{}, errInvalidEnvelope
	}
	if cancel != (msg.Method == wire.Methods.Cancel) {
		return envelope{}, errInvalidEnvelope
	}
	if !cancel {
		var err error
		if msg.ID, err = r.identity(); err != nil {
			return envelope{}, err
		}
	}
	msg.Params = r.Params
	return msg, nil
}

func (r rawEnvelope) failure() (envelope, error) {
	id, err := r.identity()
	if err != nil {
		return envelope{}, err
	}
	var problem Problem
	if r.Error.Kind() != '{' || framevalue.DecodeValue(r.Error, &problem, "error") != nil || problem.Code == "" {
		return envelope{}, errInvalidEnvelope
	}
	return envelope{ID: id, Error: &problem}, nil
}

func (r rawEnvelope) identity() (string, error) {
	var id string
	if r.ID.Kind() != '"' || json.Unmarshal(r.ID, &id) != nil {
		return "", errInvalidEnvelope
	}
	if _, ok := requestNumber(id); !ok {
		return "", errInvalidEnvelope
	}
	return id, nil
}

func requestNumber(id string) (uint64, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	return n, err == nil && n != 0 && strconv.FormatUint(n, 10) == id
}

func (p *peer) receive(msg envelope) error {
	if msg.Method == wire.Methods.Cancel && msg.ID == "" {
		return p.receiveCancellation(msg)
	}
	n, ok := requestNumber(msg.ID)
	if !ok {
		return errors.New("process: invalid message identity")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if msg.Method == "" {
		return p.receiveResponseLocked(msg, n)
	}
	return p.receiveRequestLocked(msg, n)
}

func (p *peer) receiveCancellation(msg envelope) error {
	var c cancelParams
	if msg.Result != nil || msg.Error != nil || decode(msg.Params, &c) != nil {
		return errors.New("process: invalid cancellation")
	}
	if _, ok := requestNumber(c.ID); !ok {
		return errors.New("process: invalid cancellation identity")
	}
	p.mu.Lock()
	cancel := p.running[c.ID]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// receiveResponseLocked delivers the single reply to a request this peer sent.
func (p *peer) receiveResponseLocked(msg envelope, n uint64) error {
	if msg.Params != nil || (msg.Result == nil) == (msg.Error == nil) || n > p.next || msg.Error != nil && !msg.Error.valid() {
		return errors.New("process: invalid response")
	}
	reply := p.pending[msg.ID]
	if reply == nil {
		return errors.New("process: duplicate or unknown response")
	}
	delete(p.pending, msg.ID)
	reply <- msg
	return nil
}

// receiveRequestLocked starts a request; identities must strictly increase.
func (p *peer) receiveRequestLocked(msg envelope, n uint64) error {
	if n <= p.lastIn || msg.Result != nil || msg.Error != nil || msg.Params.Kind() != '{' {
		return errors.New("process: invalid request")
	}
	p.lastIn = n
	if len(p.running) >= wire.MaxInFlight {
		return p.sendLocked(envelope{ID: msg.ID, Error: &Problem{wire.Errors.Capacity, "too many active calls"}})
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.running[msg.ID] = cancel
	p.workers.Add(1)
	go p.respond(ctx, cancel, msg)
	return nil
}

func (p *peer) respond(ctx context.Context, cancel context.CancelFunc, msg envelope) {
	defer p.workers.Done()
	defer cancel()
	result, err := p.run(ctx, msg)
	reply := envelope{ID: msg.ID, Result: result}
	if err != nil {
		reply.Result = nil
		if errors.Is(err, context.Canceled) {
			reply.Error = &Problem{wire.Errors.Cancelled, "request cancelled"}
		} else if errors.Is(err, ErrOutcomeUnknown) {
			reply.Error = &Problem{wire.Errors.Unknown, "operation did not return a confirmed result"}
		} else if established, ok := errors.AsType[*Problem](err); ok {
			reply.Error = established
		} else {
			reply.Error = &Problem{wire.Errors.Unknown, "operation did not return a confirmed result"}
		}
	}
	p.mu.Lock()
	delete(p.running, msg.ID)
	sendErr := p.sendLocked(reply)
	p.mu.Unlock()
	if sendErr != nil {
		p.end(sendErr)
	}
}
func (p *peer) run(ctx context.Context, msg envelope) (result jsontext.Value, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = ErrOutcomeUnknown
		}
	}()
	return p.handle(ctx, msg.Method, msg.Params)
}

func decode(raw jsontext.Value, out any) error {
	if err := framevalue.DecodeValue(raw, out, "wire"); err != nil {
		return &Problem{wire.Errors.Invalid, "invalid payload"}
	}
	return nil
}

func (p *peer) request(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return cancellation.Err(ctx)
	}
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	reply := make(chan envelope, 1)
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return cancellation.Err(ctx)
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
		return errors.Join(ErrOutcomeUnknown, context.Cause(p.ctx))
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
		return errors.Join(ErrOutcomeUnknown, cancellation.Err(ctx))
	}
}

func readAnswer(reply envelope, result any) error {
	if reply.Error != nil {
		return reply.Error
	}
	if err := decode(reply.Result, result); err != nil {
		return fmt.Errorf("%w: invalid reply: %v", ErrOutcomeUnknown, err)
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
