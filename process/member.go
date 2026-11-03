package process

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/nilness"
	"github.com/Tangerg/frame/portable"
	framevalue "github.com/Tangerg/frame/value"
)

// Keeping the Go types with their descriptor prevents a handler from being
// registered under a schema derived from different input or output types.
type Member[Input, Output any] struct{ method *portable.Method }

func NewMember[Input, Output any](config portable.MethodConfig) (Member[Input, Output], error) {
	method, err := portable.NewMethod[Input, Output](config)
	return Member[Input, Output]{method}, err
}
func (m Member[Input, Output]) Descriptor() portable.MethodDescriptor { return m.method.Descriptor() }

type Implementation struct {
	method *portable.Method
	invoke func(context.Context, portable.Invocation) (framevalue.JSON, error)
}

func (m Member[Input, Output]) ImplementQuery(handler func(context.Context, Input) (Output, error)) (Implementation, error) {
	if handler == nil {
		return Implementation{}, frame.ErrInvalid
	}
	return m.implement(portable.Query, func(ctx context.Context, _ string, input Input) (Output, error) {
		return handler(ctx, input)
	})
}

func (m Member[Input, Output]) ImplementCommand(handler func(context.Context, string, Input) (Output, error)) (Implementation, error) {
	return m.implement(portable.Command, handler)
}

func (m Member[Input, Output]) implement(kind portable.MethodKind, handler func(context.Context, string, Input) (Output, error)) (Implementation, error) {
	if m.method == nil || handler == nil {
		return Implementation{}, frame.ErrInvalid
	}
	if m.method.Kind() != kind {
		return Implementation{}, frame.ErrContractMismatch
	}
	return Implementation{m.method, func(ctx context.Context, invocation portable.Invocation) (framevalue.JSON, error) {
		var input Input
		if err := framevalue.DecodeValue(invocation.Input().Bytes(), &input, m.method.Name()); err != nil {
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

func (m Member[Input, Output]) Query(ctx context.Context, service Service, input Input) (Output, error) {
	return m.invoke(ctx, service, portable.Query, "", input)
}

func (m Member[Input, Output]) Command(ctx context.Context, service Service, commandID string, input Input) (Output, error) {
	return m.invoke(ctx, service, portable.Command, commandID, input)
}

func (m Member[Input, Output]) invoke(ctx context.Context, service Service, kind portable.MethodKind, commandID string, input Input) (Output, error) {
	var output Output
	if m.method == nil || nilness.Is(service) {
		return output, frame.ErrInvalid
	}
	raw, err := framevalue.EncodeJSON(input)
	if err != nil {
		return output, err
	}
	var invocation portable.Invocation
	if kind == portable.Query {
		invocation, err = portable.NewQueryInvocation(m.method.Name(), raw)
	} else {
		invocation, err = portable.NewCommandInvocation(commandID, m.method.Name(), raw)
	}
	if err != nil {
		return output, errors.Join(frame.ErrInvalid, err)
	}
	if err := m.method.ValidateInvocation(invocation); err != nil {
		return output, errors.Join(frame.ErrContractMismatch, err)
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
func Implement(contract *portable.Capability, members ...Implementation) (Service, error) {
	if contract == nil || len(members) != len(contract.Descriptor().Methods) {
		return nil, frame.ErrContractMismatch
	}
	methods := make(map[string]Implementation, len(members))
	for _, member := range members {
		if member.method == nil || member.invoke == nil {
			return nil, frame.ErrInvalid
		}
		name := member.method.Name()
		declared, exists := contract.Method(name)
		if !exists || methods[name].method != nil || declared.Identity() != member.method.Identity() {
			return nil, fmt.Errorf("%w: member %s", frame.ErrContractMismatch, name)
		}
		methods[name] = member
	}
	return Handler(func(ctx context.Context, invocation portable.Invocation) (framevalue.JSON, error) {
		m, ok := methods[invocation.Member()]
		if !ok {
			return framevalue.JSON{}, frame.ErrContractMismatch
		}
		if err := m.method.ValidateInvocation(invocation); err != nil {
			return framevalue.JSON{}, errors.Join(frame.ErrContractMismatch, err)
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
