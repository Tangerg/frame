package process

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func connectedService(t *testing.T, descriptor frame.MethodDescriptor, implementation frame.Service) *remote {
	t.Helper()
	contract, err := frame.NewCapability("test.outcome", descriptor)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	delivery := &deliveries{}
	left := newPeer(a, wire.MaxFrameBytes, func(ctx context.Context, _ string, raw jsontext.Value) (jsontext.Value, error) {
		return delivery.receive(ctx, raw)
	})
	var right *peer
	right = newPeer(b, wire.MaxFrameBytes, func(ctx context.Context, _ string, raw jsontext.Value) (jsontext.Value, error) {
		return dispatchCall(ctx, right, map[string]binding{"one": {contract, implementation}}, raw)
	})
	left.start()
	right.start()
	t.Cleanup(func() {
		left.end(context.Canceled)
		right.end(context.Canceled)
		<-left.joined
		<-right.joined
	})
	return &remote{left, "one", contract, delivery}
}

func TestStreamFailureCannotBeErasedByProducerSuccess(t *testing.T) {
	method, err := frame.NewStreamingMethod[string, string, string](frame.MethodConfig{Name: "stream", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	consumerFailure := errors.New("consumer rejected the chunk")
	input, _ := value.EncodeJSON("content")
	invalid, _ := value.EncodeJSON(1)
	for _, failure := range []string{"consumer-error", "consumer-panic", "consumer-error-panic", "invalid-chunk"} {
		t.Run(failure, func(t *testing.T) {
			implementation := testStream{func(ctx context.Context, in value.JSON, emit frame.Emit) (value.JSON, error) {
				chunk := in
				if failure == "invalid-chunk" {
					chunk = invalid
				}
				_ = emit(ctx, chunk)
				return in, nil
			}}
			service := connectedService(t, method.Descriptor(), implementation)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := service.Stream(ctx, queryInvocation(t, "stream", input), func(context.Context, value.JSON) error {
				switch failure {
				case "consumer-error":
					return consumerFailure
				case "consumer-panic":
					panic("consumer failed")
				case "consumer-error-panic":
					panic(consumerFailure)
				default:
					t.Error("invalid chunk reached the consumer")
					return nil
				}
			})
			if err == nil || result.Validate() == nil || !errors.Is(err, frame.ErrOutcomeUnknown) {
				t.Fatalf("failed delivery became a successful final result: %s %v", result.String(), err)
			}
			if (failure == "consumer-error" || failure == "consumer-error-panic") && !errors.Is(err, consumerFailure) {
				t.Fatalf("original consumer failure was lost: %v", err)
			}
			if failure == "consumer-panic" && !errors.Is(err, errConsumerPanic) {
				t.Fatalf("consumer panic was lost: %v", err)
			}
		})
	}
}

func TestStreamFinalResultJoinsPendingConsumer(t *testing.T) {
	method, err := frame.NewStreamingMethod[string, string, string](frame.MethodConfig{Name: "stream", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	entered, consumed, emitted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	service := connectedService(t, method.Descriptor(), testStream{func(ctx context.Context, in value.JSON, emit frame.Emit) (value.JSON, error) {
		go func() { defer close(emitted); _ = emit(ctx, in) }()
		<-entered
		return in, nil
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	input, _ := value.EncodeJSON("content")
	_, err = service.Stream(ctx, queryInvocation(t, "stream", input), func(ctx context.Context, _ value.JSON) error {
		close(entered)
		<-ctx.Done()
		close(consumed)
		return nil
	})
	if err == nil || !errors.Is(err, frame.ErrOutcomeUnknown) {
		t.Fatalf("pending delivery accepted a final result: %v", err)
	}
	select {
	case <-consumed:
	default:
		t.Fatal("call returned before its accepted consumer joined")
	}
	select {
	case <-emitted:
	case <-ctx.Done():
		t.Fatal("producer delivery did not join")
	}
}

func TestEscapedEmitterCannotPublishAfterFinalResult(t *testing.T) {
	method, err := frame.NewStreamingMethod[string, string, string](frame.MethodConfig{Name: "stream", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	emitter := make(chan frame.Emit, 1)
	service := connectedService(t, method.Descriptor(), testStream{func(_ context.Context, in value.JSON, emit frame.Emit) (value.JSON, error) {
		emitter <- emit
		return in, nil
	}})
	input, _ := value.EncodeJSON("content")
	result, err := service.Stream(t.Context(), queryInvocation(t, "stream", input), func(context.Context, value.JSON) error {
		t.Error("escaped emitter delivered a chunk")
		return nil
	})
	if err != nil || result != input {
		t.Fatal(result, err)
	}
	if err := (<-emitter)(t.Context(), input); !errors.Is(err, errStreamClosed) {
		t.Fatalf("escaped emitter kept delivery authority: %v", err)
	}
}

func TestHandlerFailureCannotClaimNonDispatchAfterAnEffect(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "mutate", Kind: frame.Command})
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{context.Canceled, &Problem{wire.Errors.Contract, "handler failed after its effect"}} {
		t.Run(failure.Error(), func(t *testing.T) {
			var effects atomic.Int32
			service := connectedService(t, method.Descriptor(), frame.Handler(func(context.Context, frame.Invocation) (value.JSON, error) {
				effects.Add(1)
				return value.JSON{}, failure
			}))
			input, _ := value.EncodeJSON("content")
			_, err := service.Invoke(t.Context(), commandInvocation(t, "original-effect", "mutate", input))
			if effects.Load() != 1 || !errors.Is(err, frame.ErrOutcomeUnknown) {
				t.Fatalf("effect was classified as a pre-dispatch refusal: effects=%d error=%v", effects.Load(), err)
			}
			if errors.Is(failure, context.Canceled) && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled result lost its cancellation cause: %v", err)
			}
		})
	}
}

func TestRejectionBeforeHandlerDoesNotClaimAnUnknownEffect(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "mutate", Kind: frame.Command})
	if err != nil {
		t.Fatal(err)
	}
	service := connectedService(t, method.Descriptor(), frame.Handler(func(context.Context, frame.Invocation) (value.JSON, error) {
		t.Error("rejected invocation entered the handler")
		return value.JSON{}, nil
	}))
	input, _ := value.EncodeJSON("content")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.Invoke(cancelled, commandInvocation(t, "original-effect", "mutate", input)); !errors.Is(err, context.Canceled) || errors.Is(err, frame.ErrOutcomeUnknown) {
		t.Fatalf("local pre-dispatch cancellation: %v", err)
	}
	var result value.JSON
	err = service.peer.request(t.Context(), wire.Methods.Call, callParams{Handle: service.handle, Member: "undeclared", Input: input}, &result)
	if err == nil || errors.Is(err, frame.ErrOutcomeUnknown) {
		t.Fatalf("framework pre-dispatch rejection: %v", err)
	}
}
