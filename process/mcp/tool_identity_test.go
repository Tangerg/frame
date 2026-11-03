package mcp

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func TestStandardToolRequestsCarryOnlyOriginalCommandIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := sdk.NewServer(&sdk.Implementation{Name: "identity-test", Version: "1"}, nil)
	observed := make(chan sdk.Meta, 1)
	members := make(map[string]*toolMember)
	for _, kind := range []portable.MethodKind{portable.Query, portable.Command} {
		method, err := portable.AdmitMethod(portable.MethodDescriptor{Name: string(kind), Kind: kind, Input: []byte(`{"type":"object","additionalProperties":false}`), Output: []byte(`{"type":"string"}`)})
		if err != nil {
			t.Fatal(err)
		}
		members[method.Name()] = &toolMember{method: method}
		server.AddTool(&sdk.Tool{Name: method.Name(), InputSchema: json.RawMessage(method.Descriptor().Input)}, func(_ context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			observed <- request.Params.Meta
			return &sdk.CallToolResult{StructuredContent: "ok"}, nil
		})
	}
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
	var service process.Service = &toolService{connection: &toolsConnection{session: clientSession}, members: members}
	input, err := value.ParseJSON([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, invocation := range []portable.Invocation{
		queryInvocation(t, string(portable.Query), input),
		commandInvocation(t, "original-身份", string(portable.Command), input),
	} {
		reply, err := service.Invoke(ctx, invocation)
		if err != nil || reply.String() != `"ok"` {
			t.Fatalf("tool result: %s %v", reply.String(), err)
		}
		metadata := <-observed
		raw, present := metadata[commandIDMetadataKey]
		if invocation.Kind() == portable.Query {
			if present {
				t.Fatal("query acquired a command identity")
			}
			continue
		}
		var commandID string
		if !present || json.Unmarshal(raw, &commandID) != nil || commandID != invocation.CommandID() {
			t.Fatalf("MCP metadata lost original command identity: %s", raw)
		}
	}
}
