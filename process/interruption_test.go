package process

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestHandlerGoexitReturnsUnknownAndReleasesCallCapacity(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "exit", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	service := connectedService(t, method.Descriptor(), frame.Handler(func(context.Context, frame.Invocation) (value.JSON, error) {
		runtime.Goexit()
		return value.JSON{}, nil
	}))
	input, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for attempt := range wire.MaxInFlight + 1 {
		_, err := service.Invoke(ctx, queryInvocation(t, method.Name(), input))
		if !errors.Is(err, frame.ErrOutcomeUnknown) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("handler exit %d did not return its unknown outcome: %v", attempt, err)
		}
	}
}

func TestStreamConsumerGoexitRejectsAnUnconfirmedFinalResult(t *testing.T) {
	method, err := frame.NewStreamingMethod[string, string, string](frame.MethodConfig{Name: "read", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := frame.NewCapability("test.interrupted-stream", method.Descriptor())
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
		var params callParams
		if err := decodeRequest(raw, &params); err != nil {
			return nil, err
		}
		_ = right.acknowledge(ctx, wire.Methods.Chunk, chunkParams{params.Stream, params.Input})
		return params.Input.Bytes(), nil
	})
	left.start()
	right.start()
	t.Cleanup(func() {
		left.end(context.Canceled)
		right.end(context.Canceled)
		<-left.joined
		<-right.joined
	})
	service := &remote{left, "one", contract, delivery}
	input, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := service.Stream(ctx, queryInvocation(t, method.Name(), input), func(context.Context, value.JSON) error {
		runtime.Goexit()
		return nil
	})
	if result.Validate() == nil || !errors.Is(err, frame.ErrOutcomeUnknown) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("consumer exit lost its rejection: result=%s error=%v", result.String(), err)
	}
}

func TestConsumerPanicCauseCannotBecomeAWireCancellation(t *testing.T) {
	a, b := net.Pipe()
	client := newPeer(a, wire.MaxFrameBytes, nil)
	server := newPeer(b, wire.MaxFrameBytes, func(ctx context.Context, _ string, _ jsontext.Value) (jsontext.Value, error) {
		call := newStreamCall(ctx)
		defer call.finish(nil)
		err := call.deliver(ctx, func(context.Context) error { panic(context.Canceled) })
		return nil, err
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
	err := client.acknowledge(ctx, "consume", struct{}{})
	problem, ok := errors.AsType[*Problem](err)
	if !ok || problem.Code != wire.Errors.Unknown || errors.Is(err, context.Canceled) {
		t.Fatalf("local panic cause changed the wire outcome: %v", err)
	}
}
