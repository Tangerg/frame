package process

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func admissionClient(t *testing.T, life frame.Lifetime, callback bool, handler frame.Handler) *remote {
	t.Helper()
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "write", Kind: frame.Command})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := frame.NewCapability("test.admission", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]binding{"service": {contract, handler}}
	left, right := net.Pipe()
	caller := newPeer(left, wire.MaxFrameBytes, nil)
	var receiver *peer
	if callback {
		session := &hostSession{lifetime: life, grants: bound}
		session.active.Store(true)
		receiver = newPeer(right, wire.MaxFrameBytes, session.dispatch)
		session.peer = receiver
	} else {
		server := &server{state: active, life: life, bound: bound}
		receiver = newPeer(right, wire.MaxFrameBytes, server.handle)
		server.peer = receiver
	}
	caller.start()
	receiver.start()
	t.Cleanup(func() {
		caller.end(context.Canceled)
		receiver.end(context.Canceled)
		<-caller.joined
		<-receiver.joined
	})
	return &remote{peer: caller, handle: "service", contract: contract, deliveries: &deliveries{}}
}

func TestLifetimeRejectionsStayBeforeHandlerDispatch(t *testing.T) {
	for _, direction := range []struct {
		name     string
		callback bool
	}{{"service", false}, {"host-callback", true}} {
		for _, rejection := range []struct {
			name  string
			cause error
			code  string
		}{{"capacity", frame.ErrCapacity, wire.Errors.Capacity}, {"closed", frame.ErrClosed, wire.Errors.Closed}} {
			t.Run(direction.name+"/"+rejection.name, func(t *testing.T) {
				life, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxWork: 1})
				if err != nil {
					t.Fatal(err)
				}
				defer life.Close(context.Background())
				if errors.Is(rejection.cause, frame.ErrClosed) {
					life.BeginClose(frame.ErrClosed)
				} else {
					work, err := life.Enter(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer work.Finish()
				}
				client := admissionClient(t, life, direction.callback, func(context.Context, frame.Invocation) (value.JSON, error) {
					t.Error("rejected command entered its handler")
					return value.EncodeJSON("unexpected")
				})
				input, _ := value.EncodeJSON("input")
				_, err = client.Invoke(t.Context(), commandInvocation(t, "original-command", "write", input))
				problem, ok := errors.AsType[*Problem](err)
				if !ok || problem.Code != rejection.code || errors.Is(err, frame.ErrOutcomeUnknown) {
					t.Fatalf("known pre-dispatch rejection: %v", err)
				}
			})
		}
	}
}

func TestDispatchedHandlerErrorsCannotClaimAdmissionRejection(t *testing.T) {
	for _, callback := range []bool{false, true} {
		for _, cause := range []error{frame.ErrCapacity, frame.ErrClosed} {
			life := frame.NewLifetime(t.Context())
			defer life.Close(context.Background())
			var effects atomic.Int32
			client := admissionClient(t, life, callback, func(context.Context, frame.Invocation) (value.JSON, error) {
				effects.Add(1)
				return value.JSON{}, cause
			})
			input, _ := value.EncodeJSON("input")
			_, err := client.Invoke(t.Context(), commandInvocation(t, "original-command", "write", input))
			if !errors.Is(err, frame.ErrOutcomeUnknown) || effects.Load() != 1 {
				t.Fatalf("callback=%t handler cause=%v effects=%d: %v", callback, cause, effects.Load(), err)
			}
		}
	}
}

func TestLifecycleAdmissionClassifiesOnlyEntryFailures(t *testing.T) {
	for _, operation := range []struct {
		method string
		state  phase
		raw    jsontext.Value
	}{{wire.Methods.Prepare, described, jsontext.Value(`{"imports":{}}`)}, {wire.Methods.Activate, prepared, jsontext.Value(`{}`)}} {
		for _, cause := range []error{frame.ErrCapacity, frame.ErrClosed} {
			t.Run(operation.method+"/"+cause.Error(), func(t *testing.T) {
				life, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxWork: 1})
				if err != nil {
					t.Fatal(err)
				}
				defer life.Close(context.Background())
				if errors.Is(cause, frame.ErrClosed) {
					life.BeginClose(cause)
				} else {
					work, err := life.Enter(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer work.Finish()
				}
				server := &server{life: life, state: operation.state}
				_, err = server.handle(t.Context(), operation.method, operation.raw)
				problem, ok := errors.AsType[*Problem](err)
				if !errors.Is(err, cause) || !ok || responseProblem(err).Code != problem.Code || server.state != operation.state {
					t.Fatalf("entry advanced lifecycle or lost known cause: state=%v error=%v", server.state, err)
				}
			})
		}
	}
}

func TestLifetimeAdmissionPreservesCancellationCause(t *testing.T) {
	life := frame.NewLifetime(t.Context())
	defer life.Close(context.Background())
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("request withdrawn")
	cancel(cause)
	server := &server{life: life, state: active}
	_, err := server.call(ctx, nil)
	if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) || responseProblem(err).Code != wire.Errors.Cancelled {
		t.Fatalf("cancellation cause changed: %v", err)
	}
}
