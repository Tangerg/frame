package process

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"maps"
	"os"
	"sync"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

// Unit's contracts are published by the author before a host supplies any grants.
type Unit struct {
	ID      framehost.UnitID
	Imports map[string]*frame.Capability
	Exports map[string]*frame.Capability
	Prepare func(context.Context, Setup) (Prepared, error)
}

type Import struct {
	Source  framehost.ExportRef
	Service frame.Service
}
type Setup struct {
	Lifetime frame.Lifetime
	Imports  map[string][]Import
}
type Prepared struct {
	Exports  map[string]frame.Service
	Activate func(context.Context) error
}

type phase uint8

const (
	newSession phase = iota
	described
	preparing
	prepared
	activating
	active
)

type server struct {
	unit       Unit
	peer       *peer
	life       frame.Lifetime
	deliveries deliveries
	mu         sync.Mutex
	state      phase
	bound      map[string]binding
	activate   func(context.Context) error
}

func (s *server) transition(ctx context.Context, before, after phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	if s.life.Context().Err() != nil {
		return lifetimeAdmissionError(frame.ErrClosed)
	}
	if s.state != before {
		return &Problem{wire.Errors.Invalid, "invalid lifecycle transition"}
	}
	s.state = after
	return nil
}

func (s *server) retireIncomplete(expected phase) {
	s.mu.Lock()
	incomplete := s.state == expected
	s.mu.Unlock()
	if incomplete {
		s.life.BeginClose(frame.ErrOutcomeUnknown)
	}
}

func identities(contracts map[string]*frame.Capability) map[string]string {
	result := make(map[string]string, len(contracts))
	for name, c := range contracts {
		result[name] = c.Identity().Digest().String()
	}
	return result
}

const acknowledged = "true"

func (s *server) handle(ctx context.Context, method string, raw jsontext.Value) (jsontext.Value, error) {
	switch method {
	case wire.Methods.Hello:
		return s.hello(ctx, raw)
	case wire.Methods.Prepare:
		return s.prepare(ctx, raw)
	case wire.Methods.Activate:
		return s.activateUnit(ctx, raw)
	case wire.Methods.Call:
		return s.call(ctx, raw)
	case wire.Methods.Chunk:
		return s.deliveries.receive(ctx, raw)
	case wire.Methods.Close:
		return s.close(ctx, raw)
	default:
		return nil, &Problem{wire.Errors.Invalid, "unknown operation"}
	}
}

func (s *server) hello(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
	var host hello
	if err := decodeRequest(raw, &host); err != nil {
		return nil, err
	}
	if host.Version != wire.Version || host.MaxFrameBytes <= 0 {
		return nil, mismatch()
	}
	if err := s.transition(ctx, newSession, described); err != nil {
		return nil, err
	}
	limit := min(host.MaxFrameBytes, wire.MaxFrameBytes)
	if err := s.peer.reduceFrame(limit); err != nil {
		return nil, err
	}
	return json.Marshal(description{wire.Version, limit, string(s.unit.ID), identities(s.unit.Exports), identities(s.unit.Imports)})
}

func (s *server) activateUnit(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
	if err := decodeRequest(raw, &struct{}{}); err != nil {
		return nil, err
	}
	work, err := s.life.Enter(ctx)
	if err != nil {
		return nil, lifetimeAdmissionError(err)
	}
	defer work.Finish()
	if err := s.transition(work.Context(), prepared, activating); err != nil {
		return nil, err
	}
	defer s.retireIncomplete(activating)
	if s.activate != nil {
		if err := s.activate(work.Context()); err != nil {
			return nil, err
		}
	}
	if err := s.transition(work.Context(), activating, active); err != nil {
		return nil, err
	}
	return jsontext.Value(acknowledged), nil
}

func (s *server) call(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
	s.mu.Lock()
	ready := s.state == active
	s.mu.Unlock()
	if !ready {
		return nil, &Problem{wire.Errors.Closed, "instance is not active"}
	}
	work, err := s.life.Enter(ctx)
	if err != nil {
		return nil, lifetimeAdmissionError(err)
	}
	defer work.Finish()
	return dispatchCall(work.Context(), s.peer, s.bound, raw)
}

func (s *server) close(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
	if err := decodeRequest(raw, &struct{}{}); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.state != active && s.state != activating {
		s.mu.Unlock()
		return nil, &Problem{wire.Errors.Invalid, "invalid lifecycle transition"}
	}
	s.mu.Unlock()
	outcome, err := s.life.Close(ctx)
	if err != nil || !outcome.Released {
		return nil, errors.Join(frame.ErrOutcomeUnknown, err)
	}
	return jsontext.Value(acknowledged), nil
}

func (s *server) prepare(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
	var request preparation
	if err := decodeRequest(raw, &request); err != nil {
		return nil, err
	}
	if request.Imports == nil || len(request.Imports) != len(s.unit.Imports) {
		return nil, mismatch()
	}
	work, err := s.life.Enter(ctx)
	if err != nil {
		return nil, lifetimeAdmissionError(err)
	}
	defer work.Finish()
	if err := s.transition(work.Context(), described, preparing); err != nil {
		return nil, err
	}
	defer s.retireIncomplete(preparing)
	setup := Setup{Lifetime: s.life, Imports: map[string][]Import{}}
	for name, contract := range s.unit.Imports {
		grants, ok := request.Imports[name]
		if !ok || grants == nil {
			return nil, mismatch()
		}
		imports := make([]Import, 0, len(grants))
		for _, grant := range grants {
			if !grant.validates(contract) {
				return nil, mismatch()
			}
			imports = append(imports, Import{grant.Source, &remote{s.peer, grant.Handle, contract, &s.deliveries}})
		}
		setup.Imports[name] = imports
	}
	p, err := s.unit.Prepare(work.Context(), setup)
	if err != nil {
		return nil, err
	}
	if len(p.Exports) != len(s.unit.Exports) {
		return nil, mismatch()
	}
	bound := make(map[string]binding, len(p.Exports))
	for name, contract := range s.unit.Exports {
		svc := p.Exports[name]
		if framevalue.IsNil(svc) {
			return nil, mismatch()
		}
		bound[name] = binding{contract, svc}
	}
	s.mu.Lock()
	s.bound = bound
	s.activate = p.Activate
	s.mu.Unlock()
	if err := s.transition(work.Context(), preparing, prepared); err != nil {
		return nil, err
	}
	return jsontext.Value(acknowledged), nil
}

// Serve owns the duplex channel until disconnection and joins accepted plugin work.
func Serve(ctx context.Context, channel io.ReadWriteCloser, unit Unit) error {
	if framevalue.IsNil(ctx) || framevalue.IsNil(channel) || unit.ID.Validate() != nil || unit.Prepare == nil {
		return frame.ErrInvalid
	}
	unit.Imports = maps.Clone(unit.Imports)
	unit.Exports = maps.Clone(unit.Exports)
	for _, set := range []map[string]*frame.Capability{unit.Imports, unit.Exports} {
		for name, contract := range set {
			if !frame.ValidName(name) || contract == nil {
				return frame.ErrInvalid
			}
			captured := new(*contract)
			if captured.Identity().Validate() != nil {
				return frame.ErrInvalid
			}
			set[name] = captured
		}
	}
	s := &server{unit: unit, life: frame.NewLifetime(ctx)}
	s.peer = newPeer(channel, wire.MaxFrameBytes, s.handle)
	s.peer.start()
	stop := context.AfterFunc(ctx, func() { s.peer.end(frame.ContextError(ctx)) })
	<-s.peer.done
	stop()
	s.life.BeginClose(context.Cause(s.peer.ctx))
	<-s.peer.joined
	_, cleanupErr := s.life.Close(context.Background())
	cause := context.Cause(s.peer.ctx)
	if errors.Is(cause, io.EOF) {
		cause = nil
	}
	return errors.Join(cause, cleanupErr)
}

type stdio struct{}

func (stdio) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (stdio) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
func (stdio) Close() error                { return errors.Join(os.Stdin.Close(), os.Stdout.Close()) }

func ServeStdio(ctx context.Context, unit Unit) error {
	return Serve(ctx, stdio{}, unit)
}
