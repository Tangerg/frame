package browser

import (
	"context"
	"reflect"

	"github.com/Tangerg/frame"
)

// methodName is the wire name of one control method.
type methodName string

const (
	admitPlanMethod     methodName = "admitPlan"
	readyMethod         methodName = "ready"
	acquireMethod       methodName = "acquire"
	validateUseMethod   methodName = "validateUse"
	validateRouteMethod methodName = "validateRoute"
	snapshotMethod      methodName = "snapshot"
	closeSnapshotMethod methodName = "closeSnapshot"
	publishMethod       methodName = "publish"
	publicationMethod   methodName = "publication"
	withdrawMethod      methodName = "withdraw"
	proveMethod         methodName = "prove"
	reconnectMethod     methodName = "reconnect"
	pollMethod          methodName = "poll"
)

// methodEffect tells a Runtime whether repeating a lost request needs the
// original operation identity.
type methodEffect uint8

const (
	observes methodEffect = iota
	mutates
)

type controlMethod struct {
	name          methodName
	effect        methodEffect
	input, output reflect.Type
	handle        func(*Bridge, context.Context, string, HTTPEnvelope) (any, error)
}

func requestMethod[I, O any](name methodName, effect methodEffect, call func(*Bridge, context.Context, string, I) (O, error)) controlMethod {
	return controlMethod{name, effect, reflect.TypeFor[I](), reflect.TypeFor[O](), func(b *Bridge, ctx context.Context, token string, envelope HTTPEnvelope) (any, error) {
		input, err := decodeInput[I](envelope.Input)
		if err != nil {
			return nil, err
		}
		return call(b, ctx, token, input)
	}}
}

func acknowledgement[I any](name methodName, effect methodEffect, call func(*Bridge, context.Context, string, I) error) controlMethod {
	method := requestMethod(name, effect, func(b *Bridge, ctx context.Context, token string, input I) (any, error) {
		return nil, call(b, ctx, token, input)
	})
	method.output = nil
	return method
}

var controlMethods = []controlMethod{
	requestMethod(admitPlanMethod, mutates, (*Bridge).AdmitPlan),
	requestMethod(readyMethod, mutates, (*Bridge).Ready),
	requestMethod(acquireMethod, mutates, (*Bridge).Acquire),
	acknowledgement(validateUseMethod, observes, (*Bridge).ValidateUse),
	acknowledgement(validateRouteMethod, observes, (*Bridge).ValidateRoute),
	requestMethod(snapshotMethod, mutates, (*Bridge).Snapshot),
	acknowledgement(closeSnapshotMethod, mutates, (*Bridge).CloseSnapshot),
	requestMethod(publishMethod, mutates, (*Bridge).Publish),
	requestMethod(publicationMethod, observes, (*Bridge).Publication),
	requestMethod(withdrawMethod, mutates, (*Bridge).Withdraw),
	requestMethod(proveMethod, mutates, (*Bridge).Prove),
	requestMethod(reconnectMethod, mutates, (*Bridge).Reconnect),
	requestMethod(pollMethod, observes, func(b *Bridge, ctx context.Context, token string, _ struct{}) (Poll, error) {
		return b.Poll(ctx, token)
	}),
}

func (b *Bridge) dispatch(ctx context.Context, token string, request HTTPEnvelope) (any, error) {
	for _, method := range controlMethods {
		if string(method.name) == request.Method {
			return method.handle(b, ctx, token, request)
		}
	}
	return nil, frame.ErrInvalid
}
