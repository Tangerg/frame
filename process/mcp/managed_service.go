package mcp

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	framevalue "github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

type managedInvocationKey struct{}

// WithInvocation supplies caller-owned target evidence independently of command identity.
func WithInvocation(ctx context.Context, invocation Invocation) (context.Context, error) {
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
	lifetime   *frame.Lifetime
	openCall   OpenManagedCall
	openStream OpenManagedStream
	session    *sdk.ClientSession
	physical   string
	source     frame.ExportRef
	contract   *ManagedContract
	receipt    *portable.Method
}

func (s *ManagedService) params(ctx context.Context, method *portable.Method, input framevalue.JSON) (*managedParams, error) {
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

const maxCommandIDCharacters = 256

func validCommandID(id string) bool {
	return id != "" && utf8.ValidString(id) && utf8.RuneCountInString(id) <= maxCommandIDCharacters
}

// Queries need no durable command identity. Mutations must use Command so that
// the original business owner can reconcile a missing response without replay.
func (s *ManagedService) Call(ctx context.Context, member string, input framevalue.JSON) (framevalue.JSON, error) {
	return s.invoke(ctx, portable.Query, "", member, input, nil)
}

func (s *ManagedService) Command(ctx context.Context, id, member string, input framevalue.JSON) (framevalue.JSON, error) {
	if !validCommandID(id) {
		return framevalue.JSON{}, fmt.Errorf("%w: bounded command ID required", frame.ErrInvalid)
	}
	return s.invoke(ctx, portable.Mutation, id, member, input, nil)
}

func (s *ManagedService) invoke(ctx context.Context, kind portable.MethodKind, id, member string, input framevalue.JSON, emit process.Emit) (output framevalue.JSON, returned error) {
	if ctx == nil {
		return framevalue.JSON{}, frame.ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return framevalue.JSON{}, err
	}
	method, ok := s.contract.capability.Method(member)
	if !ok || method.Kind() != kind || (len(method.Descriptor().Stream) != 0) != (emit != nil) || (emit != nil && s.openStream == nil) {
		return framevalue.JSON{}, fmt.Errorf("%w: managed member %q does not support this invocation", frame.ErrContractMismatch, member)
	}
	params, err := s.params(ctx, method, input)
	if err != nil {
		return framevalue.JSON{}, err
	}
	params.CommandID = id
	if s.openCall != nil {
		lease, err := reserveManagedLease(s.lifetime)
		if err != nil {
			return framevalue.JSON{}, err
		}
		grant, err := lease.issue(func() (*ManagedCallback, error) {
			return s.openCall(ctx, s.session, ManagedCall{Source: s.source, Method: method, Input: input, CommandID: id})
		})
		defer func() { returned = errors.Join(returned, lease.finish(ctx)) }()
		if err != nil {
			return framevalue.JSON{}, err
		}
		if grant != nil {
			if err := grant.attach(&params.ParamsBase, managedCallback); err != nil {
				return framevalue.JSON{}, err
			}
		}
	}
	var raw jsontext.Value
	if err := context.Cause(ctx); err != nil {
		return framevalue.JSON{}, err
	}
	if emit != nil {
		raw, err = s.stream(ctx, method, params, emit)
	} else {
		raw, err = managedRaw(ctx, s.session, managedInvoke, params)
		if err != nil {
			err = managedDispatchError(errors.Join(err, context.Cause(ctx)))
		}
	}
	if err == nil {
		err = managedDispatchError(method.ValidateOutput(raw))
	}
	if err != nil {

		return framevalue.JSON{}, err
	}
	return framevalue.ParseJSON(raw)
}

func managedDispatchError(err error) error {
	if err != nil {
		if _, refused := errors.AsType[*Refusal](err); !refused {
			return errors.Join(process.ErrOutcomeUnknown, err)
		}
	}
	return err
}

// ReadReceipt observes the backend's durable reply for the original admitted
// input. Missing evidence never authorizes a new command.
func (s *ManagedService) ReadReceipt(ctx context.Context, id, member string, input framevalue.JSON) (framevalue.JSON, bool, error) {
	method, ok := s.contract.capability.Method(member)
	if !ok || method.Kind() != portable.Mutation || !validCommandID(id) || input.Validate() != nil || s.receipt == nil {
		return framevalue.JSON{}, false, fmt.Errorf("%w: exact receipt query and command evidence required", frame.ErrContractMismatch)
	}
	request, err := framevalue.EncodeJSON(struct {
		ID          string            `json:"id"`
		Method      string            `json:"method"`
		InputDigest framevalue.Digest `json:"inputDigest"`
	}{id, member, input.Digest()})
	if err != nil {
		return framevalue.JSON{}, false, err
	}
	params, err := s.params(ctx, s.receipt, request)
	if err != nil {
		return framevalue.JSON{}, false, err
	}
	raw, err := managedRaw(ctx, s.session, managedInvoke, params)
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
	raw, err = reply.unpack()
	if err != nil {
		_, refused := errors.AsType[*Refusal](err)
		return framevalue.JSON{}, refused, err
	}
	if err := method.ValidateOutput(raw); err != nil {
		return framevalue.JSON{}, false, err
	}
	value, err := framevalue.ParseJSON(raw)
	return value, err == nil, err
}
