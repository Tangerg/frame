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
	"github.com/Tangerg/frame/portable"
)

// Unit's contracts are published by the author before a host supplies any grants.
type Unit struct {
	ID      frame.UnitID
	Imports map[string]*portable.Capability
	Exports map[string]*portable.Capability
	Prepare func(context.Context, Setup) (Prepared, error)
}

type Import struct {
	Source  frame.ExportRef
	Service Service
}
type Setup struct {
	Lifetime *frame.Lifetime
	Imports  map[string][]Import
}
type Prepared struct {
	Exports  map[string]Service
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
	closed
)

type server struct {
	unit       Unit
	peer       *peer
	life       *frame.Lifetime
	deliveries deliveries
	mu         sync.Mutex
	state      phase
	bound      map[string]binding
	activate   func(context.Context) error
}

func (s *server) transition(before, after phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != before {
		return &Problem{profile.Errors.Invalid, "invalid lifecycle transition"}
	}
	s.state = after
	return nil
}

func identities(contracts map[string]*portable.Capability) map[string]string {
	result := make(map[string]string, len(contracts))
	for name, c := range contracts {
		result[name] = c.Identity().Digest().String()
	}
	return result
}
func (s *server) handle(ctx context.Context, method string, raw jsontext.Value) (jsontext.Value, error) {
	switch method {
	case profile.Methods.Hello:
		var host hello
		if err := decode(raw, &host); err != nil {
			return nil, err
		}
		if host.Version != profile.Version || host.MaxFrameBytes <= 0 {
			return nil, mismatch()
		}
		if err := s.transition(newSession, described); err != nil {
			return nil, err
		}
		limit := min(host.MaxFrameBytes, profile.MaxFrameBytes)
		if err := s.peer.reduceFrame(limit); err != nil {
			return nil, err
		}
		return json.Marshal(description{profile.Version, limit, string(s.unit.ID), identities(s.unit.Exports), identities(s.unit.Imports)})
	case profile.Methods.Prepare:
		return s.prepare(ctx, raw)
	case profile.Methods.Activate:
		if err := decode(raw, &struct{}{}); err != nil {
			return nil, err
		}
		work, err := s.life.Enter(ctx)
		if err != nil {
			return nil, err
		}
		defer work.Finish()
		if err := s.transition(prepared, activating); err != nil {
			return nil, err
		}
		if s.activate != nil {
			if err := s.activate(work.Context()); err != nil {
				s.mu.Lock()
				s.state = closed
				s.mu.Unlock()
				return nil, err
			}
		}
		if err := s.transition(activating, active); err != nil {
			return nil, err
		}
		return jsontext.Value("true"), nil
	case profile.Methods.Call:
		s.mu.Lock()
		ready := s.state == active
		s.mu.Unlock()
		if !ready {
			return nil, &Problem{profile.Errors.Closed, "instance is not active"}
		}
		work, err := s.life.Enter(ctx)
		if err != nil {
			return nil, err
		}
		defer work.Finish()
		return dispatchCall(work.Context(), s.peer, s.bound, raw)
	case profile.Methods.Chunk:
		return s.deliveries.receive(ctx, raw)
	case profile.Methods.Close:
		if err := decode(raw, &struct{}{}); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.state != active && s.state != activating {
			s.mu.Unlock()
			return nil, &Problem{profile.Errors.Invalid, "invalid lifecycle transition"}
		}
		s.state = closed
		s.mu.Unlock()
		outcome, err := s.life.Close(ctx)
		if err != nil || !outcome.Released {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		return jsontext.Value("true"), nil
	default:
		return nil, &Problem{profile.Errors.Invalid, "unknown operation"}
	}
}

func (s *server) prepare(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
	var request preparation
	if err := decode(raw, &request); err != nil {
		return nil, err
	}
	if request.Imports == nil || len(request.Imports) != len(s.unit.Imports) {
		return nil, mismatch()
	}
	if err := s.transition(described, preparing); err != nil {
		return nil, err
	}
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
	p, err := s.unit.Prepare(ctx, setup)
	if err != nil {
		return nil, err
	}
	if len(p.Exports) != len(s.unit.Exports) {
		return nil, mismatch()
	}
	bound := make(map[string]binding, len(p.Exports))
	for name, contract := range s.unit.Exports {
		svc := p.Exports[name]
		if nilDependency(svc) {
			return nil, mismatch()
		}
		bound[name] = binding{contract, svc}
	}
	s.mu.Lock()
	s.bound = bound
	s.activate = p.Activate
	s.state = prepared
	s.mu.Unlock()
	return jsontext.Value("true"), nil
}

// Serve owns the duplex channel until disconnection and joins accepted plugin work.
func Serve(ctx context.Context, channel io.ReadWriteCloser, unit Unit) error {
	if ctx == nil || nilDependency(channel) || unit.ID.Validate() != nil || unit.Prepare == nil {
		return frame.ErrInvalid
	}
	for _, set := range []map[string]*portable.Capability{unit.Imports, unit.Exports} {
		for name, contract := range set {
			if !frame.ValidName(name) || contract == nil {
				return frame.ErrInvalid
			}
		}
	}
	unit.Imports = maps.Clone(unit.Imports)
	unit.Exports = maps.Clone(unit.Exports)
	s := &server{unit: unit, life: frame.NewLifetime(ctx)}
	s.peer = newPeer(channel, profile.MaxFrameBytes, s.handle)
	s.peer.start()
	stop := context.AfterFunc(ctx, func() { s.peer.end(context.Cause(ctx)) })
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
