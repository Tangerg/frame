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
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

type hello struct {
	Protocol      framevalue.Digest `json:"protocol"`
	MaxFrameBytes int               `json:"maxFrameBytes"`
}

// description answers hello with the negotiated terms and the capability
// identities the unit was built against, keyed by export and import slot.
type description struct {
	hello
	Unit    string            `json:"unit"`
	Exports map[string]string `json:"exports"`
	Imports map[string]string `json:"imports"`
}

func describe(terms hello, unit string, exports, imports map[string]*frame.Capability) description {
	return description{hello: terms, Unit: unit, Exports: identities(exports), Imports: identities(imports)}
}

func identities(contracts map[string]*frame.Capability) map[string]string {
	result := make(map[string]string, len(contracts))
	for name, contract := range contracts {
		result[name] = contract.Identity().Digest().String()
	}
	return result
}

type offered struct {
	Handle        string                 `json:"handle"`
	Source        framehost.ExportRef    `json:"source"`
	ValueContract *framehost.ContractRef `json:"valueContract,omitzero"`
}

func (g offered) validates(contract *frame.Capability) bool {
	if !frame.ValidName(g.Handle) || g.Source.Validate() != nil {
		return false
	}
	ref := g.Source.Contract
	if ref.Kind == framehost.PointKind && ref.Mode == framehost.PortableMode {
		if g.ValueContract == nil {
			return false
		}
		ref = *g.ValueContract
	} else if g.ValueContract != nil {
		return false
	}
	return ref.Validate() == nil && ref.Mode == framehost.PortableMode && ref.Kind == framehost.ServiceKind && ref.Name == contract.Descriptor().Reference && ref.Digest == contract.Identity().Digest()
}

type preparation struct {
	Imports map[string][]offered `json:"imports"`
}

type hostSession struct {
	peer       *peer
	active     atomic.Bool
	lifetime   frame.Lifetime
	grants     map[string]boundService
	deliveries deliveries
}

func (s *hostSession) dispatch(ctx context.Context, method string, raw jsontext.Value) (jsontext.Value, error) {
	if method == wire.Methods.Chunk {
		return s.deliveries.receive(ctx, raw)
	}
	if method != wire.Methods.Call || !s.active.Load() {
		return nil, &Problem{wire.Errors.Closed, "instance is not active"}
	}
	work, err := s.lifetime.Enter(ctx)
	if err != nil {
		return nil, lifetimeAdmissionError(err)
	}
	defer work.Finish()
	return dispatchCall(work.Context(), s.peer, s.grants, raw)
}

type localProtocol struct{}

func (localProtocol) Profile() framehost.Profile { return framehost.LocalProcess }

// admitFrameLimit refuses a driver frame larger than the published local wire
// accepts, before any setup can negotiate it.
func (localProtocol) admitFrameLimit(maxFrameBytes int) error {
	if maxFrameBytes > wire.MaxFrameBytes {
		return fmt.Errorf("%w: local process frame exceeds %d bytes", frame.ErrInvalid, wire.MaxFrameBytes)
	}
	return nil
}

type assembly struct {
	setup    framehost.Setup
	session  *hostSession
	expected description
	grants   preparation
	values   []framehost.ExportValue
}

func (localProtocol) Prepare(setup framehost.Setup, metadata ExportMetadata, maxFrameBytes int) (Connection, error) {
	s := &hostSession{}
	s.peer = newPeer(nil, maxFrameBytes, s.dispatch)
	s.grants = map[string]boundService{}
	s.lifetime = setup.Lifetime()
	unit := setup.Unit()
	exports := make(map[string]*frame.Capability, len(unit.Exports))
	values := make([]framehost.ExportValue, 0, len(unit.Exports))
	for _, export := range unit.Exports {
		contract, err := setup.Capability(export.Contract)
		if err != nil {
			return nil, err
		}
		exports[export.Name] = contract
		values = append(values, metadata.Export(export, &remote{s.peer, export.Name, contract, &s.deliveries}))
	}
	imports := make(map[string]*frame.Capability, len(unit.Imports))
	prepared := preparation{Imports: map[string][]offered{}}
	for _, slot := range unit.Imports {
		contract, err := setup.Capability(slot.Contract)
		if err != nil {
			return nil, err
		}
		imports[slot.Slot] = contract
		offers, err := s.offer(setup, slot.Slot, contract)
		if err != nil {
			return nil, err
		}
		prepared.Imports[slot.Slot] = offers
	}
	expected := describe(hello{Protocol: protocol, MaxFrameBytes: maxFrameBytes}, string(unit.ID), exports, imports)
	return &assembly{setup, s, expected, prepared, values}, nil
}

// offer grants the child one handle per provider the Host bound to slot. A
// provider inside the same unit is served back through the child itself.
func (s *hostSession) offer(setup framehost.Setup, slot string, contract *frame.Capability) ([]offered, error) {
	resolved, err := setup.Imports(slot)
	if err != nil {
		return nil, err
	}
	offers := make([]offered, 0, len(resolved))
	for _, provider := range resolved {
		svc, ok := provider.Value.(frame.Service)
		if provider.Local {
			svc = &remote{s.peer, provider.Source.Export, contract, &s.deliveries}
		} else if !ok || framevalue.IsNil(svc) {
			return nil, fmt.Errorf("%w: slot %s requires a portable service", frame.ErrContractMismatch, slot)
		}
		handle := rand.Text()
		s.grants[handle] = boundService{contract, svc}
		offer := offered{Handle: handle, Source: provider.Source}
		if provider.Source.Contract.Kind == framehost.PointKind {
			value, err := setup.ValueContract(provider.Source.Contract)
			if err != nil {
				return nil, err
			}
			offer.ValueContract = &value
		}
		offers = append(offers, offer)
	}
	return offers, nil
}

func (a *assembly) Connect(ctx context.Context, ch io.ReadWriteCloser) ([]framehost.ExportValue, error) {
	s, expected := a.session, a.expected
	s.peer.transport = ch
	s.peer.start()
	var actual description
	if err := s.peer.request(ctx, wire.Methods.Hello, expected.hello, &actual); err != nil {
		return nil, err
	}
	if actual.Protocol != expected.Protocol || actual.Unit != expected.Unit || actual.Exports == nil || actual.Imports == nil || !maps.Equal(actual.Exports, expected.Exports) || !maps.Equal(actual.Imports, expected.Imports) {
		return nil, fmt.Errorf("%w: process handshake differs from declared unit", frame.ErrContractMismatch)
	}
	if err := s.peer.reduceFrame(actual.MaxFrameBytes); err != nil {
		return nil, err
	}
	if err := s.peer.acknowledge(ctx, wire.Methods.Prepare, a.grants); err != nil {
		return nil, err
	}
	if err := a.setup.OnActivate(func(ctx context.Context) error {
		s.active.Store(true)
		err := s.peer.acknowledge(ctx, wire.Methods.Activate, struct{}{})
		if err != nil {
			s.active.Store(false)
		}
		return err
	}); err != nil {
		return nil, err
	}
	return a.values, nil
}

func (a *assembly) Close(ctx context.Context) error { return a.session.close(ctx) }

func (s *hostSession) close(ctx context.Context) error {
	wasActive := s.active.Swap(false)
	var err error
	if wasActive && s.peer.ctx.Err() == nil {
		err = s.peer.acknowledge(ctx, wire.Methods.Close, struct{}{})
	}
	s.peer.end(io.EOF)
	<-s.peer.joined
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
