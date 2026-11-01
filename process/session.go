package process

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync/atomic"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
)

type hello struct {
	Version       int `json:"version"`
	MaxFrameBytes int `json:"maxFrameBytes"`
}
type description struct {
	Version       int               `json:"version"`
	MaxFrameBytes int               `json:"maxFrameBytes"`
	Unit          string            `json:"unit"`
	Exports       map[string]string `json:"exports"`
	Imports       map[string]string `json:"imports"`
}
type offered struct {
	Handle        string             `json:"handle"`
	Source        frame.ExportRef    `json:"source"`
	ValueContract *frame.ContractRef `json:"valueContract,omitzero"`
}

func (g offered) validates(contract *portable.Capability) bool {
	if !frame.ValidName(g.Handle) || g.Source.Validate() != nil {
		return false
	}
	ref := g.Source.Contract
	if ref.Kind == frame.PointKind && ref.Mode == frame.PortableMode {
		if g.ValueContract == nil {
			return false
		}
		ref = *g.ValueContract
	} else if g.ValueContract != nil {
		return false
	}
	return ref.Validate() == nil && ref.Mode == frame.PortableMode && ref.Kind == frame.ServiceKind && ref.Name == contract.Descriptor().Reference && ref.Digest == contract.Identity().Digest()
}

type preparation struct {
	Imports map[string][]offered `json:"imports"`
}

type hostSession struct {
	peer       *peer
	active     atomic.Bool
	lifetime   *frame.Lifetime
	grants     map[string]binding
	deliveries deliveries
}

func declaredCapability(setup *frame.Setup, ref frame.ContractRef) (*portable.Capability, error) {
	service, err := setup.ValueContract(ref)
	if err != nil {
		return nil, err
	}
	if service.Mode != frame.PortableMode || service.Kind != frame.ServiceKind {
		return nil, frame.ErrContractMismatch
	}
	raw, err := setup.ContractDescriptor(service)
	if err != nil {
		return nil, err
	}
	contract, err := portable.ParseCapability(raw)
	if err != nil {
		return nil, errors.Join(frame.ErrContractMismatch, err)
	}
	if contract.Descriptor().Reference != service.Name || contract.Identity().Digest() != service.Digest {
		return nil, frame.ErrContractMismatch
	}
	return contract, nil
}

func (s *hostSession) dispatch(ctx context.Context, method string, raw jsontext.Value) (jsontext.Value, error) {
	if method == profile.Methods.Chunk {
		return s.deliveries.receive(ctx, raw)
	}
	if method != profile.Methods.Call || !s.active.Load() {
		return nil, &Problem{profile.Errors.Closed, "instance is not active"}
	}
	work, err := s.lifetime.Enter(ctx)
	if err != nil {
		return nil, &Problem{profile.Errors.Closed, "instance is retiring"}
	}
	defer work.Finish()
	return dispatchCall(work.Context(), s.peer, s.grants, raw)
}

type assembly struct {
	session  *hostSession
	expected description
	grants   preparation
	values   []frame.ExportValue
}

// Freeze and validate every representable import before creating any process.
func assemble(setup *frame.Setup, program Program, maximum int) (*assembly, error) {
	s := &hostSession{}
	s.peer = newPeer(nil, maximum, s.dispatch)
	s.grants = map[string]binding{}
	s.lifetime = setup.Lifetime()
	expected := description{Version: profile.Version, MaxFrameBytes: maximum, Unit: string(setup.Unit().ID), Exports: map[string]string{}, Imports: map[string]string{}}
	prepared := preparation{Imports: map[string][]offered{}}
	values := make([]frame.ExportValue, 0, len(setup.Unit().Exports))
	for _, export := range setup.Unit().Exports {
		contract, err := declaredCapability(setup, export.Contract)
		if err != nil {
			return nil, err
		}
		expected.Exports[export.Name] = contract.Identity().Digest().String()
		var meta any
		if value, ok := program.Metadata[export.Name]; ok {
			meta = value
		}
		values = append(values, frame.ExportValue{Name: export.Name, Contract: export.Contract, Metadata: meta, Value: &remote{s.peer, export.Name, contract, &s.deliveries}, Durable: true})
	}
	for _, slot := range setup.Unit().Imports {
		contract, err := declaredCapability(setup, slot.Contract)
		if err != nil {
			return nil, err
		}
		expected.Imports[slot.Slot] = contract.Identity().Digest().String()
		resolved, err := setup.Imports(slot.Slot)
		if err != nil {
			return nil, err
		}
		handles := make([]offered, 0, len(resolved))
		for _, provider := range resolved {
			svc, ok := provider.Value.(Service)
			if provider.Local {
				svc = &remote{s.peer, provider.Source.Export, contract, &s.deliveries}
			} else if !ok || nilDependency(svc) {
				return nil, fmt.Errorf("%w: slot %s requires a portable service", frame.ErrContractMismatch, slot.Slot)
			}
			handle := rand.Text()
			s.grants[handle] = binding{contract, svc}
			offer := offered{Handle: handle, Source: provider.Source}
			if provider.Source.Contract.Kind == frame.PointKind {
				value, err := setup.ValueContract(provider.Source.Contract)
				if err != nil {
					return nil, err
				}
				offer.ValueContract = &value
			}
			handles = append(handles, offer)
		}
		prepared.Imports[slot.Slot] = handles
	}
	return &assembly{s, expected, prepared, values}, nil
}

func (a *assembly) connect(ctx context.Context, ch io.ReadWriteCloser, setup *frame.Setup) ([]frame.ExportValue, error) {
	s, expected := a.session, a.expected
	s.peer.transport = ch
	s.peer.start()
	var actual description
	if err := s.peer.request(ctx, profile.Methods.Hello, hello{profile.Version, expected.MaxFrameBytes}, &actual); err != nil {
		return nil, err
	}
	if actual.Version != expected.Version || actual.Unit != expected.Unit || actual.Exports == nil || actual.Imports == nil || !maps.Equal(actual.Exports, expected.Exports) || !maps.Equal(actual.Imports, expected.Imports) {
		return nil, fmt.Errorf("%w: process handshake differs from declared unit", frame.ErrContractMismatch)
	}
	if err := s.peer.reduceFrame(actual.MaxFrameBytes); err != nil {
		return nil, err
	}
	if err := s.peer.acknowledge(ctx, profile.Methods.Prepare, a.grants); err != nil {
		return nil, err
	}
	if err := setup.OnActivate(func(ctx context.Context) error {
		s.active.Store(true)
		err := s.peer.acknowledge(ctx, profile.Methods.Activate, struct{}{})
		if err != nil {
			s.active.Store(false)
		}
		return err
	}); err != nil {
		return nil, err
	}
	return a.values, nil
}

func (s *hostSession) close(ctx context.Context) error {
	wasActive := s.active.Swap(false)
	var err error
	if wasActive && s.peer.ctx.Err() == nil {
		err = s.peer.acknowledge(ctx, profile.Methods.Close, struct{}{})
	}
	s.peer.end(io.EOF)
	select {
	case <-s.peer.joined:
	case <-ctx.Done():
		return errors.Join(err, context.Cause(ctx))
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
