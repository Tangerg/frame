package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func TestInputRequiredCannotReplayAnInvocation(t *testing.T) {
	for _, kind := range []frame.MethodKind{frame.Query, frame.Command} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			method, err := frame.AdmitMethod(frame.MethodDescriptor{
				Name: "invoke", Kind: kind,
				Input: []byte(`{"type":"object","additionalProperties":false}`), Output: []byte(`{"type":"boolean"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := sdk.NewServer(&sdk.Implementation{Name: "input-required-test", Version: "1"}, nil)
			server.AddTool(&sdk.Tool{Name: method.Name(), InputSchema: json.RawMessage(method.Descriptor().Input)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				if calls.Add(1) == 1 {
					return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{}, RequestState: "resume"}, nil
				}
				return &sdk.CallToolResult{StructuredContent: true}, nil
			})
			clientPipe, serverPipe := net.Pipe()
			serverSession, err := server.Connect(ctx, &sdk.IOTransport{Reader: serverPipe, Writer: serverPipe}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer serverSession.Close()
			clientSession, err := connect(ctx, clientPipe, 1<<20, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer clientSession.Close()
			service := &toolService{
				connection: &toolsConnection{session: clientSession},
				members:    map[string]*toolMember{method.Name(): {method: method}},
			}
			input, err := value.ParseJSON([]byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			invocation := queryInvocation(t, method.Name(), input)
			if kind == frame.Command {
				invocation = commandInvocation(t, "original-command", method.Name(), input)
			}
			result, err := service.Invoke(ctx, invocation)
			if calls.Load() != 1 || result.Validate() == nil || !errors.Is(err, frame.ErrOutcomeUnknown) {
				t.Fatalf("input round changed dispatch policy: calls=%d, result=%s, error=%v", calls.Load(), result.String(), err)
			}
		})
	}
}
