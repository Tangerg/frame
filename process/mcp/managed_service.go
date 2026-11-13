package mcp

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

type managedInvocationKey struct{}

// WithInvocation supplies caller-owned target evidence independently of command identity.
func WithInvocation(ctx context.Context, invocation Invocation) (context.Context, error) {
	if framevalue.IsNil(ctx) {
		return nil, frame.ErrInvalid
	}
	if err := invocation.Validate(); err != nil {
		return nil, err
	}
	if prior, exists := ctx.Value(managedInvocationKey{}).(Invocation); exists && prior != invocation {
		return nil, errors.New("mcp: invocation already bound")
	}
	return context.WithValue(ctx, managedInvocationKey{}, invocation), nil
}

// ManagedService is a physical export bound to one verified session. Commands
// require a caller-owned identity so transport failure never creates a new effect.
type ManagedService struct {
	lifetime   frame.Lifetime
	openCall   OpenManagedCall
	openStream OpenManagedStream
	session    *sdk.ClientSession
	physical   string
	source     framehost.ExportRef
	contract   *ManagedContract
	receipt    *frame.Method
}

func (s *ManagedService) params(ctx context.Context, method *frame.Method, input framevalue.JSON) (*managedParams, error) {
	if framevalue.IsNil(ctx) {
		return nil, frame.ErrInvalid
	}
	if err := method.ValidateInput(input); err != nil {
		return nil, errors.Join(frame.ErrContractMismatch, err)
	}
	params := &managedParams{Protocol: managedVersion, Method: method.Name(), Contract: method.Identity().Digest(), Export: s.physical, Capability: s.contract.wire.Digest(), Input: input.Bytes()}
	if invocation, exists := ctx.Value(managedInvocationKey{}).(Invocation); exists {
		if method.Name() != managedReceipt && invocation.Target.Scope != method.Descriptor().Target {
			return nil, fmt.Errorf("%w: managed invocation target differs", frame.ErrContractMismatch)
		}
		if err := params.Set(managedInvocation, invocation); err != nil {
			return nil, err
		}
	}
	return params, nil
}

func (s *ManagedService) ValidateInput(ctx context.Context, member string, input framevalue.JSON) error {
	method, ok := s.contract.capability.Method(member)
	if !ok {
		return frame.ErrContractMismatch
	}
	params, err := s.params(ctx, method, input)
	if err != nil {
		return err
	}
	var problem string
	if err := managedCall(ctx, s.session, managedValidate, params, &problem); err != nil {
		return err
	}
	if problem != "" {
		return fmt.Errorf("%w: %s", frame.ErrContractMismatch, problem)
	}
	return nil
}

func (s *ManagedService) Invoke(ctx context.Context, invocation frame.Invocation) (framevalue.JSON, error) {
	return s.invoke(ctx, invocation, nil)
}

func (s *ManagedService) invoke(ctx context.Context, invocation frame.Invocation, emit frame.Emit) (output framevalue.JSON, returned error) {
	if err := frame.ContextError(ctx); err != nil {
		return framevalue.JSON{}, err
	}
	method, ok := s.contract.capability.Method(invocation.Member())
	if !ok || (len(method.Descriptor().Stream) != 0) != (emit != nil) || (emit != nil && s.openStream == nil) {
		return framevalue.JSON{}, fmt.Errorf("%w: managed member %q does not support this invocation", frame.ErrContractMismatch, invocation.Member())
	}
	if err := method.ValidateInvocation(invocation); err != nil {
		return framevalue.JSON{}, errors.Join(frame.ErrContractMismatch, err)
	}
	params, err := s.params(ctx, method, invocation.Input())
	if err != nil {
		return framevalue.JSON{}, err
	}
	params.CommandID = invocation.CommandID()
	finish, err := s.grantCallback(ctx, method, invocation, &params.ParamsBase)
	defer func() { returned = errors.Join(returned, finish()) }()
	if err != nil {
		return framevalue.JSON{}, err
	}
	if err := frame.ContextError(ctx); err != nil {
		return framevalue.JSON{}, err
	}
	return s.dispatch(ctx, method, params, emit)
}

// grantCallback lets the backend call back into Frame while this invocation
// runs. The returned finish must run after the call settles, even on error.
func (s *ManagedService) grantCallback(ctx context.Context, method *frame.Method, invocation frame.Invocation, metadata *sdk.ParamsBase) (finish func() error, err error) {
	finish = func() error { return nil }
	if s.openCall == nil {
		return finish, nil
	}
	lease, err := reserveManagedLease(s.lifetime)
	if err != nil {
		return finish, err
	}
	finish = func() error { return lease.finish(ctx) }
	grant, err := lease.issue(func() (*ManagedCallback, error) {
		return s.openCall(ctx, s.session, ManagedCall{Source: s.source, Method: new(*method), Input: invocation.Input(), CommandID: invocation.CommandID()})
	})
	if err != nil || grant == nil {
		return finish, err
	}
	return finish, grant.attach(metadata, managedCallback)
}

func (s *ManagedService) dispatch(ctx context.Context, method *frame.Method, params *managedParams, emit frame.Emit) (framevalue.JSON, error) {
	var reply managedReply
	var err error
	if emit != nil {
		reply, err = s.stream(ctx, method, params, emit)
	} else {
		reply, err = requestManaged(ctx, s.session, managedInvoke, params)
		if err != nil {
			err = errors.Join(frame.ErrOutcomeUnknown, err)
		}
	}
	raw, refusal := reply.result()
	if refusal != nil {
		return framevalue.JSON{}, errors.Join(refusal, err)
	}
	if len(raw) == 0 && err != nil {
		return framevalue.JSON{}, err
	}
	if validationErr := method.ValidateOutput(raw); validationErr != nil {
		return framevalue.JSON{}, errors.Join(frame.ErrOutcomeUnknown, err, validationErr)
	}
	output, parseErr := framevalue.ParseJSON(raw)
	if parseErr != nil {
		return framevalue.JSON{}, errors.Join(frame.ErrOutcomeUnknown, err, parseErr)
	}
	return output, err
}

// ReadReceipt observes the backend's durable reply for the original admitted
// input. Missing evidence never authorizes a new command.
func (s *ManagedService) ReadReceipt(ctx context.Context, invocation frame.Invocation) (framevalue.JSON, bool, error) {
	method, ok := s.contract.capability.Method(invocation.Member())
	if !ok || invocation.Kind() != frame.Command || method.ValidateInvocation(invocation) != nil || s.receipt == nil {
		return framevalue.JSON{}, false, fmt.Errorf("%w: exact receipt query and command evidence required", frame.ErrContractMismatch)
	}
	request, err := framevalue.EncodeJSON(struct {
		ID          string            `json:"id"`
		Method      string            `json:"method"`
		InputDigest framevalue.Digest `json:"inputDigest"`
	}{invocation.CommandID(), invocation.Member(), invocation.Input().Digest()})
	if err != nil {
		return framevalue.JSON{}, false, err
	}
	params, err := s.params(ctx, s.receipt, request)
	if err != nil {
		return framevalue.JSON{}, false, err
	}
	received, err := requestManaged(ctx, s.session, managedInvoke, params)
	if err != nil {
		return framevalue.JSON{}, false, err
	}
	raw, err := received.result()
	if err != nil {
		return framevalue.JSON{}, false, err
	}
	if err := s.receipt.ValidateOutput(raw); err != nil {
		return framevalue.JSON{}, false, err
	}
	var receipt struct {
		Found bool           `json:"found"`
		Reply jsontext.Value `json:"reply,omitzero"`
	}
	if err := framevalue.DecodeValue(raw, &receipt, "managed receipt"); err != nil {
		return framevalue.JSON{}, false, err
	}
	if receipt.Found != (len(receipt.Reply) != 0) {
		return framevalue.JSON{}, false, errors.New("mcp: inconsistent managed receipt")
	}
	if !receipt.Found {
		return framevalue.JSON{}, false, nil
	}
	var reply managedResult
	if err := reply.UnmarshalJSON(receipt.Reply); err != nil {
		return framevalue.JSON{}, false, err
	}
	settled, err := reply.unpack()
	if err != nil {
		return framevalue.JSON{}, false, err
	}
	raw, err = settled.result()
	if err != nil {
		return framevalue.JSON{}, true, err
	}
	if err := method.ValidateOutput(raw); err != nil {
		return framevalue.JSON{}, false, err
	}
	value, err := framevalue.ParseJSON(raw)
	return value, err == nil, err
}
