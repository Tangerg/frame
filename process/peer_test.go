package process

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame/portable"
	framevalue "github.com/Tangerg/frame/value"
)

func TestDuplexStreamingAllowsNestedCallsWithAcknowledgedChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	method, err := portable.NewStreamingMethod[string, string, string](portable.MethodConfig{Name: "read", Kind: portable.Query})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := portable.NewCapability("test.stream", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	delivery := &deliveries{}
	left := newPeer(a, profile.MaxFrameBytes, func(ctx context.Context, method string, data jsontext.Value) (jsontext.Value, error) {
		if method == profile.Methods.Chunk {
			return delivery.receive(ctx, data)
		}
		return nil, mismatch()
	})
	var called atomic.Int32
	implementation := testStream{func(ctx context.Context, chunk framevalue.JSON, emit Emit) (framevalue.JSON, error) {
		if err := emit(ctx, chunk); err != nil {
			return framevalue.JSON{}, err
		}
		if called.Load() != 1 {
			t.Error("chunk was acknowledged before its consumer returned")
		}
		return chunk, nil
	}}
	var right *peer
	right = newPeer(b, profile.MaxFrameBytes, func(ctx context.Context, _ string, raw jsontext.Value) (jsontext.Value, error) {
		return dispatchCall(ctx, right, map[string]binding{"one": {contract, implementation}}, raw)
	})
	left.start()
	right.start()
	defer left.end(context.Canceled)
	defer right.end(context.Canceled)
	svc := &remote{left, "one", contract, delivery}
	in, _ := framevalue.EncodeJSON("value")
	got, err := svc.Stream(ctx, "read", in, func(_ context.Context, value framevalue.JSON) error {
		if value != in {
			t.Error("changed chunk")
		}
		called.Add(1)
		return nil
	})
	if err != nil || got != in {
		t.Fatal(got, err)
	}
}

type testStream struct {
	emit func(context.Context, framevalue.JSON, Emit) (framevalue.JSON, error)
}

func (testStream) Call(context.Context, string, framevalue.JSON) (framevalue.JSON, error) {
	return framevalue.JSON{}, mismatch()
}
func (s testStream) Stream(ctx context.Context, _ string, input framevalue.JSON, emit Emit) (framevalue.JSON, error) {
	return s.emit(ctx, input, emit)
}

func TestCancellationTargetsOriginalCall(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started, finished := make(chan struct{}), make(chan struct{})
	a, b := net.Pipe()
	left := newPeer(a, profile.MaxFrameBytes, nil)
	right := newPeer(b, profile.MaxFrameBytes, func(ctx context.Context, _ string, _ jsontext.Value) (jsontext.Value, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return nil, context.Cause(ctx)
	})
	left.start()
	right.start()
	defer left.end(context.Canceled)
	defer right.end(context.Canceled)
	go func() { <-started; cancel() }()
	err := left.acknowledge(ctx, "wait", struct{}{})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("original handler not cancelled")
	}
}

func TestNegotiatedLimitAppliesBeforeTerminator(t *testing.T) {
	a, b := net.Pipe()
	p := newPeer(a, profile.MaxFrameBytes, nil)
	p.start()
	defer b.Close()
	defer p.end(context.Canceled)
	if err := p.reduceFrame(32); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("oversized unterminated frame held the session")
	}
}

func TestProtocolDiscriminatorsCannotBeForgedWithNull(t *testing.T) {
	for _, data := range []string{
		`{"id":"1","method":null,"result":true}`,
		`{"id":"1","error":null,"result":true}`,
		`{"id":"1","error":{"code":"invalid_request"}}`,
		`{"method":"cancel","id":null,"params":{"id":"1"}}`,
		`{"id":"1","method":"hello","params":null}`,
		`{"id":"1","result":true,"result":false}`,
	} {
		if _, err := parseEnvelope([]byte(data)); err == nil {
			t.Errorf("admitted %s", data)
		}
	}
	valid, err := parseEnvelope([]byte(`{"id":"1","result":null}`))
	if err != nil || string(valid.Result) != "null" {
		t.Fatalf("legitimate null lost: %s %v", valid.Result, err)
	}
}
