package process

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
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
	call   func(context.Context, framevalue.JSON) (framevalue.JSON, error)
}

func (m Member[Input, Output]) Implement(handler func(context.Context, Input) (Output, error)) Implementation {
	if m.method == nil || handler == nil {
		return Implementation{}
	}
	return Implementation{m.method, func(ctx context.Context, raw framevalue.JSON) (framevalue.JSON, error) {
		var input Input
		if err := framevalue.DecodeValue(raw.Bytes(), &input, m.method.Name()); err != nil {
			return framevalue.JSON{}, err
		}
		output, err := handler(ctx, input)
		if err != nil {
			return framevalue.JSON{}, err
		}
		return framevalue.EncodeJSON(output)
	}}
}
func (m Member[Input, Output]) Call(ctx context.Context, service Service, input Input) (Output, error) {
	var output Output
	if m.method == nil || nilDependency(service) {
		return output, frame.ErrInvalid
	}
	raw, err := framevalue.EncodeJSON(input)
	if err != nil {
		return output, err
	}
	if err = m.method.ValidateInput(raw); err != nil {
		return output, err
	}
	reply, err := service.Call(ctx, m.method.Name(), raw)
	if err != nil {
		return output, err
	}
	if err = m.method.ValidateOutput(reply.Bytes()); err != nil {
		return output, errors.Join(ErrOutcomeUnknown, err)
	}
	err = framevalue.DecodeValue(reply.Bytes(), &output, m.method.Name())
	return output, err
}

// Partial implementations cannot publish a usable handle for the full contract.
func Implement(contract *portable.Capability, members ...Implementation) (Service, error) {
	if contract == nil || len(members) != len(contract.Descriptor().Methods) {
		return nil, frame.ErrContractMismatch
	}
	methods := make(map[string]Implementation, len(members))
	for _, member := range members {
		if member.method == nil || member.call == nil {
			return nil, frame.ErrInvalid
		}
		name := member.method.Name()
		declared, exists := contract.Method(name)
		if !exists || methods[name].method != nil || declared.Identity() != member.method.Identity() {
			return nil, fmt.Errorf("%w: member %s", frame.ErrContractMismatch, name)
		}
		methods[name] = member
	}
	return Handler(func(ctx context.Context, name string, input framevalue.JSON) (framevalue.JSON, error) {
		m, ok := methods[name]
		if !ok {
			return framevalue.JSON{}, frame.ErrContractMismatch
		}
		if err := m.method.ValidateInput(input); err != nil {
			return framevalue.JSON{}, err
		}
		value, err := m.call(ctx, input)
		if err != nil {
			return framevalue.JSON{}, err
		}
		if err = m.method.ValidateOutput(value.Bytes()); err != nil {
			return framevalue.JSON{}, errors.Join(ErrOutcomeUnknown, err)
		}
		return value, nil
	}), nil
}
