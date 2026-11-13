package process

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestReplyProjectionFailureRetainsItsOriginalCause(t *testing.T) {
	for _, reply := range []struct {
		name string
		body jsontext.Value
	}{
		{name: "missing member", body: jsontext.Value(`{}`)},
		{name: "null member", body: jsontext.Value(`{"ready":null}`)},
	} {
		t.Run(reply.name, func(t *testing.T) {
			local, remote := net.Pipe()
			client := newPeer(local, wire.MaxFrameBytes, nil)
			server := newPeer(remote, wire.MaxFrameBytes, func(context.Context, string, jsontext.Value) (jsontext.Value, error) {
				return reply.body, nil
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
			result := struct {
				Ready bool `json:"ready"`
			}{Ready: true}
			err := client.request(ctx, wire.Methods.Prepare, struct{}{}, &result)
			shape, ok := errors.AsType[*value.ShapeError](err)
			if !errors.Is(err, frame.ErrOutcomeUnknown) || !ok || shape.Field != "ready" {
				t.Fatalf("reply failure lost its classification or cause: %v", err)
			}
			if !result.Ready {
				t.Fatal("invalid reply partially replaced the original result")
			}
		})
	}
}
