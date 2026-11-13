package frame

import (
	"context"
	"errors"
	"fmt"

	framevalue "github.com/Tangerg/frame/value"
)

// Keeping the Go types with their descriptor prevents a handler from being
// registered under a schema derived from different input or output types.
type TypedMethod[Input, Output any] struct{ method *Method }

func NewTypedMethod[Input, Output any](config MethodConfig) (TypedMethod[Input, Output], error) {
	method, err := deriveMethod[Input, Output](config, nil)
	return TypedMethod[Input, Output]{method}, err
}
func (m TypedMethod[Input, Output]) Descriptor() MethodDescriptor { return m.method.Descriptor() }

type Implementation struct {
	method *Method
	invoke func(context.Context, Invocation) (framevalue.JSON, error)
}

func (m TypedMethod[Input, Output]) ImplementQuery(handler func(context.Context, Input) (Output, error)) (Implementation, error) {
	if handler == nil {
		return Implementation{}, ErrInvalid
	}
	return m.implement(Query, func(ctx context.Context, _ string, input Input) (Output, error) {
		return handler(ctx, input)
	})
}

func (m TypedMethod[Input, Output]) ImplementCommand(handler func(context.Context, string, Input) (Output, error)) (Implementation, error) {
	return m.implement(Command, handler)
}

func (m TypedMethod[Input, Output]) implement(kind MethodKind, handler func(context.Context, string, Input) (Output, error)) (Implementation, error) {
	if m.method == nil || handler == nil {
		return Implementation{}, ErrInvalid
	}
	if m.method.Kind() != kind {
		return Implementation{}, ErrContractMismatch
	}
	return Implementation{m.method, func(ctx context.Context, invocation Invocation) (framevalue.JSON, error) {
		if err := ContextError(ctx); err != nil {
			return framevalue.JSON{}, err
		}
		var input Input
		if err := framevalue.DecodeValue(invocation.Input().Bytes(), &input, m.method.Name()); err != nil {
			return framevalue.JSON{}, err
		}
		if err := ContextError(ctx); err != nil {
			return framevalue.JSON{}, err
		}
		output, err := handler(ctx, invocation.CommandID(), input)
		if err != nil {
			return framevalue.JSON{}, errors.Join(ErrOutcomeUnknown, err)
		}
		encoded, err := framevalue.EncodeJSON(output)
		if err != nil {
			return framevalue.JSON{}, errors.Join(ErrOutcomeUnknown, err)
		}
		return encoded, nil
	}}, nil
}

func (m TypedMethod[Input, Output]) Query(ctx context.Context, service Service, input Input) (Output, error) {
	return m.invoke(ctx, service, Query, "", input)
}

func (m TypedMethod[Input, Output]) Command(ctx context.Context, service Service, commandID string, input Input) (Output, error) {
	return m.invoke(ctx, service, Command, commandID, input)
}

func (m TypedMethod[Input, Output]) invoke(ctx context.Context, service Service, kind MethodKind, commandID string, input Input) (Output, error) {
	var output Output
	if m.method == nil || framevalue.IsNil(service) {
		return output, ErrInvalid
	}
	if err := ContextError(ctx); err != nil {
		return output, err
	}
	raw, err := framevalue.EncodeJSON(input)
	if err != nil {
		return output, err
	}
	var invocation Invocation
	if kind == Query {
		invocation, err = NewQueryInvocation(m.method.Name(), raw)
	} else {
		invocation, err = NewCommandInvocation(commandID, m.method.Name(), raw)
	}
	if err != nil {
		return output, errors.Join(ErrInvalid, err)
	}
	if err := m.method.ValidateInvocation(invocation); err != nil {
		return output, errors.Join(ErrContractMismatch, err)
	}
	if err := ContextError(ctx); err != nil {
		return output, err
	}
	reply, err := service.Invoke(ctx, invocation)
	if err != nil {
		return output, err
	}
	if err = m.method.ValidateOutput(reply.Bytes()); err != nil {
		return output, errors.Join(ErrOutcomeUnknown, err)
	}
	if err := framevalue.DecodeValue(reply.Bytes(), &output, m.method.Name()); err != nil {
		return output, errors.Join(ErrOutcomeUnknown, err)
	}
	return output, nil
}

// Partial implementations cannot publish a usable handle for the full contract.
func Implement(contract *Capability, members ...Implementation) (Service, error) {
	if contract == nil || contract.Identity().Validate() != nil || len(members) != len(contract.Descriptor().Methods) {
		return nil, ErrContractMismatch
	}
	methods := make(map[string]Implementation, len(members))
	for _, member := range members {
		if member.method == nil || member.invoke == nil {
			return nil, ErrInvalid
		}
		name := member.method.Name()
		declared, exists := contract.Method(name)
		if !exists || methods[name].method != nil || declared.Identity() != member.method.Identity() {
			return nil, fmt.Errorf("%w: member %s", ErrContractMismatch, name)
		}
		methods[name] = member
	}
	return Handler(func(ctx context.Context, invocation Invocation) (framevalue.JSON, error) {
		m, ok := methods[invocation.Member()]
		if !ok {
			return framevalue.JSON{}, ErrContractMismatch
		}
		if err := m.method.ValidateInvocation(invocation); err != nil {
			return framevalue.JSON{}, errors.Join(ErrContractMismatch, err)
		}
		value, err := m.invoke(ctx, invocation)
		if err != nil {
			return framevalue.JSON{}, err
		}
		if err = m.method.ValidateOutput(value.Bytes()); err != nil {
			return framevalue.JSON{}, errors.Join(ErrOutcomeUnknown, err)
		}
		return value, nil
	}), nil
}
