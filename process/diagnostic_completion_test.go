package process

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type interruptedDiagnostic struct {
	method    string
	interrupt func()
}

func (e interruptedDiagnostic) check(method string) {
	if e.method == method {
		e.interrupt()
	}
}

func (e interruptedDiagnostic) Error() string {
	e.check("Error")
	return "handler failure"
}

func (e interruptedDiagnostic) Is(error) bool {
	e.check("Is")
	return false
}

func (e interruptedDiagnostic) As(any) bool {
	e.check("As")
	return false
}

func (e interruptedDiagnostic) Unwrap() error {
	e.check("Unwrap")
	return nil
}

func TestDiagnosticInterruptionCompletesOriginalRequests(t *testing.T) {
	for _, interruption := range []struct {
		name string
		run  func()
	}{
		{"panic", func() { panic("diagnostic interrupted") }},
		{"goexit", runtime.Goexit},
	} {
		for _, method := range []string{"Error", "Is", "As", "Unwrap"} {
			t.Run(interruption.name+"/"+method, func(t *testing.T) {
				failure := interruptedDiagnostic{method: method, interrupt: interruption.run}
				left, right := net.Pipe()
				client := newPeer(left, wire.MaxFrameBytes, nil)
				var calls atomic.Int32
				server := newPeer(right, wire.MaxFrameBytes, func(context.Context, string, jsontext.Value) (jsontext.Value, error) {
					calls.Add(1)
					return nil, failure
				})
				client.start()
				server.start()
				t.Cleanup(func() {
					client.end(context.Canceled)
					server.end(context.Canceled)
					<-client.joined
					<-server.joined
				})
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				for attempt := range wire.MaxInFlight + 1 {
					err := client.acknowledge(ctx, wire.Methods.Activate, struct{}{})
					problem, ok := errors.AsType[*Problem](err)
					if !ok || problem.Code != wire.Errors.Unknown {
						t.Fatalf("request %d did not complete with an unknown outcome: %v", attempt, err)
					}
				}
				if calls.Load() != int32(wire.MaxInFlight+1) {
					t.Fatalf("handler execution count = %d", calls.Load())
				}
				server.mu.Lock()
				remaining := len(server.running)
				server.mu.Unlock()
				if remaining != 0 {
					t.Fatalf("completed requests retained %d slots", remaining)
				}
			})
		}
	}
}

func TestCommandDiagnosticFailureDoesNotReplayItsEffect(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "mutate", Kind: frame.Command})
	if err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	service := connectedService(t, method.Descriptor(), frame.Handler(func(context.Context, frame.Invocation) (value.JSON, error) {
		effects.Add(1)
		return value.JSON{}, interruptedDiagnostic{method: "Is", interrupt: runtime.Goexit}
	}))
	input, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := service.Invoke(ctx, commandInvocation(t, "original-command", method.Name(), input))
	if effects.Load() != 1 || result.Validate() == nil || !errors.Is(err, frame.ErrOutcomeUnknown) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("command completion lost its original effect: effects=%d result=%s error=%v", effects.Load(), result.String(), err)
	}
}

func TestStreamDiagnosticFailureRetainsItsCauseAndRejectsProducerSuccess(t *testing.T) {
	method, err := frame.NewStreamingMethod[string, string, string](frame.MethodConfig{Name: "read", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	failure := &interruptedDiagnostic{method: "Is", interrupt: runtime.Goexit}
	service := connectedService(t, method.Descriptor(), testStream{func(ctx context.Context, in value.JSON, emit frame.Emit) (value.JSON, error) {
		_ = emit(ctx, in)
		return in, nil
	}})
	input, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := service.Stream(ctx, queryInvocation(t, method.Name(), input), func(context.Context, value.JSON) error {
		return failure
	})
	retained, ok := errors.AsType[*interruptedDiagnostic](err)
	if !ok || retained != failure || result.Validate() == nil || !errors.Is(err, frame.ErrOutcomeUnknown) || ctx.Err() != nil {
		t.Fatal("interrupted diagnostic lost the original stream failure or admitted a result")
	}
}

func TestInterruptedReplyAfterDisconnectionReleasesOriginalSlot(t *testing.T) {
	for _, interruption := range []struct {
		name string
		run  func()
	}{
		{"panic", func() { panic("diagnostic interrupted") }},
		{"goexit", runtime.Goexit},
	} {
		t.Run(interruption.name, func(t *testing.T) {
			entered, released := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			left, right := net.Pipe()
			client := newPeer(left, wire.MaxFrameBytes, nil)
			server := newPeer(right, wire.MaxFrameBytes, func(context.Context, string, jsontext.Value) (jsontext.Value, error) {
				close(entered)
				<-released
				return nil, interruptedDiagnostic{method: "Is", interrupt: interruption.run}
			})
			client.start()
			server.start()
			t.Cleanup(func() {
				release()
				client.end(context.Canceled)
				server.end(context.Canceled)
				<-client.joined
				<-server.joined
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- client.acknowledge(ctx, wire.Methods.Activate, struct{}{}) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("request did not enter its handler")
			}
			server.end(context.Canceled)
			release()
			if err := <-result; !errors.Is(err, frame.ErrOutcomeUnknown) {
				t.Fatalf("disconnected invocation lost its unknown outcome: %v", err)
			}
			client.end(context.Canceled)
			select {
			case <-server.joined:
			case <-ctx.Done():
				t.Fatal("interrupted worker prevented the original peer from joining")
			}
			<-client.joined
			if len(server.running) != 0 {
				t.Fatalf("closed peer retained %d request slots", len(server.running))
			}
		})
	}
}
