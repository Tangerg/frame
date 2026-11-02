package browser

import (
	"context"
	"reflect"

	"github.com/Tangerg/frame"
)

const (
	acquireOperation  = "acquire"
	publishOperation  = "publish"
	withdrawOperation = "withdraw"
)

type controlMethod struct {
	name          string
	input, output reflect.Type
	mutates       bool
	handle        func(*Bridge, context.Context, string, HTTPEnvelope) (any, error)
}

func requestMethod[I, O any](name string, mutates bool, call func(*Bridge, context.Context, string, I) (O, error)) controlMethod {
	return controlMethod{name, reflect.TypeFor[I](), reflect.TypeFor[O](), mutates, func(b *Bridge, ctx context.Context, token string, envelope HTTPEnvelope) (any, error) {
		input, err := decodeInput[I](envelope.Input)
		if err != nil {
			return nil, err
		}
		return call(b, ctx, token, input)
	}}
}
func acknowledgement[I any](name string, mutates bool, call func(*Bridge, context.Context, string, I) error) controlMethod {
	method := requestMethod(name, mutates, func(b *Bridge, ctx context.Context, token string, input I) (any, error) {
		return nil, call(b, ctx, token, input)
	})
	method.output = nil
	return method
}

var controlMethods = []controlMethod{
	acknowledgement("validatePlan", false, (*Bridge).ValidatePlan),
	requestMethod("ready", true, (*Bridge).Ready),
	requestMethod(acquireOperation, true, (*Bridge).Acquire),
	acknowledgement("validateUse", false, (*Bridge).ValidateUse),
	acknowledgement("validateRoute", false, (*Bridge).ValidateRoute),
	requestMethod("snapshot", false, (*Bridge).Snapshot),
	acknowledgement("closeSnapshot", true, (*Bridge).CloseSnapshot),
	requestMethod(publishOperation, true, (*Bridge).Publish),
	requestMethod("publication", false, (*Bridge).Publication),
	requestMethod(withdrawOperation, true, (*Bridge).Withdraw),
	requestMethod("prove", true, (*Bridge).Prove),
	requestMethod("reconnect", true, (*Bridge).Reconnect),
	requestMethod("poll", false, func(b *Bridge, ctx context.Context, token string, _ struct{}) (Poll, error) {
		return b.Poll(ctx, token)
	}),
}

func (b *Bridge) dispatch(ctx context.Context, token string, request HTTPEnvelope) (any, error) {
	for _, method := range controlMethods {
		if method.name == request.Method {
			return method.handle(b, ctx, token, request)
		}
	}
	return nil, frame.ErrInvalid
}
