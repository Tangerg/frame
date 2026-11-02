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
	if p.Code == profile.Errors.Cancelled {
		return target == context.Canceled || target == ErrOutcomeUnknown
	}
	return p.Code == profile.Errors.Unknown && target == ErrOutcomeUnknown
}

func (p *Problem) valid() bool {
	switch p.Code {
	case profile.Errors.Invalid, profile.Errors.Capacity, profile.Errors.Contract,
		profile.Errors.Unknown, profile.Errors.Closed, profile.Errors.Cancelled:
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

type cancellation struct {
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
	return &peer{transport: channel, handle: handle, ctx: ctx, stop: stop, done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}), joined: make(chan struct{}),
		out: make(chan []byte, profile.MaxInFlight*2), maxFrame: maxFrame,
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
		return &Problem{profile.Errors.Capacity, "frame exceeds limit"}
	}
	if err := p.ctx.Err(); err != nil {
		return context.Cause(p.ctx)
	}
	select {
	case p.out <- append(data, '\n'):
		return nil
	default:
		return &Problem{profile.Errors.Capacity, "output queue full"}
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

// Preserve field presence even for JSON null. Generic struct decoding alone
// collapses an explicit null discriminator into the absence of that field.
func parseEnvelope(raw []byte) (envelope, error) {
	var fields map[string]jsontext.Value
	var msg envelope
	if err := json.Unmarshal(raw, &fields); err != nil {
		return msg, err
	}
	invalid := errors.New("process: invalid envelope fields")
	id, hasID := fields["id"]
	if hasID {
		if id.Kind() != '"' || json.Unmarshal(id, &msg.ID) != nil {
			return msg, invalid
		}
		if _, ok := requestNumber(msg.ID); !ok {
			return msg, invalid
		}
	}
	if op, request := fields["method"]; request {
		if op.Kind() != '"' || json.Unmarshal(op, &msg.Method) != nil || msg.Method == "" {
			return msg, invalid
		}
		msg.Params = fields["params"]
		if msg.Params.Kind() != '{' {
			return msg, invalid
		}
		if msg.Method == profile.Methods.Cancel {
			if hasID || len(fields) != 2 {
				return msg, invalid
			}
		} else if !hasID || len(fields) != 3 {
			return msg, invalid
		}
		return msg, nil
	}
	if !hasID || len(fields) != 2 {
		return msg, invalid
	}
	if result, ok := fields["result"]; ok {
		msg.Result = result
		return msg, nil
	}
	if problem, ok := fields["error"]; ok {
		var value Problem
		if problem.Kind() != '{' || framevalue.DecodeValue(problem, &value, "error") != nil || value.Code == "" {
			return msg, invalid
		}
		msg.Error = &value
		return msg, nil
	}
	return msg, invalid
}

func requestNumber(id string) (uint64, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	return n, err == nil && n != 0 && strconv.FormatUint(n, 10) == id
}

func (p *peer) receive(msg envelope) error {
	if msg.Method == profile.Methods.Cancel && msg.ID == "" {
		var c cancellation
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
	n, ok := requestNumber(msg.ID)
	if !ok {
		return errors.New("process: invalid message identity")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if msg.Method == "" {
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
	if n <= p.lastIn || msg.Result != nil || msg.Error != nil || msg.Params.Kind() != '{' {
		return errors.New("process: invalid request")
	}
	p.lastIn = n
	if len(p.running) >= profile.MaxInFlight {
		return p.sendLocked(envelope{ID: msg.ID, Error: &Problem{profile.Errors.Capacity, "too many active calls"}})
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
			reply.Error = &Problem{profile.Errors.Cancelled, "request cancelled"}
		} else if errors.Is(err, ErrOutcomeUnknown) {
			reply.Error = &Problem{profile.Errors.Unknown, "operation did not return a confirmed result"}
		} else if established, ok := errors.AsType[*Problem](err); ok {
			reply.Error = established
		} else {
			reply.Error = &Problem{profile.Errors.Unknown, "operation did not return a confirmed result"}
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
		return &Problem{profile.Errors.Invalid, "invalid payload"}
	}
	return nil
}

func (p *peer) request(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	reply := make(chan envelope, 1)
	p.mu.Lock()
	if len(p.pending) >= profile.MaxInFlight || p.next == ^uint64(0) {
		p.mu.Unlock()
		return &Problem{profile.Errors.Capacity, "request limit reached"}
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
		data, _ := json.Marshal(cancellation{id})
		err := p.sendLocked(envelope{Method: profile.Methods.Cancel, Params: data})
		p.mu.Unlock()
		if err != nil {
			p.end(err)
		}
		return errors.Join(ErrOutcomeUnknown, context.Cause(ctx))
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
