package process

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestServeRetainsTheCapabilityItDescribed(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "read", Kind: frame.Query})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := frame.NewCapability("test.capture", method.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	admitted := *contract
	a, b := net.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- Serve(context.WithoutCancel(t.Context()), a, Unit{ID: "worker", Exports: map[string]*frame.Capability{"reader": contract}, Prepare: func(context.Context, Setup) (Prepared, error) {
			return Prepared{Exports: map[string]frame.Service{"reader": frame.Handler(func(_ context.Context, invocation frame.Invocation) (value.JSON, error) {
				return invocation.Input(), nil
			})}}, nil
		}})
	}()
	client := newPeer(b, wire.MaxFrameBytes, nil)
	client.start()
	t.Cleanup(func() {
		client.end(io.EOF)
		<-client.joined
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var actual description
	if err := client.request(ctx, wire.Methods.Hello, hello{wire.Version, wire.MaxFrameBytes}, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Exports["reader"] != admitted.Identity().Digest().String() {
		t.Fatal("handshake changed the declared contract")
	}
	replacement, err := frame.NewMethod[int, bool](frame.MethodConfig{Name: "read", Kind: frame.Command})
	if err != nil {
		t.Fatal(err)
	}
	replacementContract, err := frame.NewCapability("test.capture", replacement.Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	*contract = *replacementContract
	if err := client.acknowledge(ctx, wire.Methods.Prepare, preparation{Imports: map[string][]offered{}}); err != nil {
		t.Fatal(err)
	}
	if err := client.acknowledge(ctx, wire.Methods.Activate, struct{}{}); err != nil {
		t.Fatal(err)
	}
	input, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	service := remote{peer: client, handle: "reader", contract: &admitted}
	result, err := service.Invoke(ctx, queryInvocation(t, method.Name(), input))
	if err != nil || result != input {
		t.Fatalf("caller replaced the described service: result=%s error=%v", result.String(), err)
	}
}
