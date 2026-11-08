package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	"github.com/Tangerg/go-sdk/jsonrpc"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func catalogContract(t *testing.T, kind portable.MethodKind) (*portable.Method, *sdk.Tool) {
	t.Helper()
	method, err := portable.AdmitMethod(portable.MethodDescriptor{
		Name: "invoke", Kind: kind,
		Input:  []byte(`{"type":"object","additionalProperties":false}`),
		Output: []byte(`{"type":"object","required":["accepted"],"properties":{"accepted":{"type":"boolean"}},"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := &sdk.Tool{
		Name: method.Name(), InputSchema: json.RawMessage(method.Descriptor().Input),
		OutputSchema: json.RawMessage(method.Descriptor().Output),
		Annotations:  &sdk.ToolAnnotations{ReadOnlyHint: kind == portable.Query},
	}
	return method, tool
}

func connectCatalogOverPipe(t *testing.T, ctx context.Context, server *sdk.Server, method *portable.Method, pages int) (*toolService, error) {
	t.Helper()
	clientPipe, serverPipe := net.Pipe()
	t.Cleanup(func() { _ = clientPipe.Close(); _ = serverPipe.Close() })
	serverSession, err := server.Connect(ctx, &sdk.IOTransport{Reader: serverPipe, Writer: serverPipe}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	member := &toolMember{method: method}
	required := make(toolRequirements)
	if err := required.admit("service", member); err != nil {
		t.Fatal(err)
	}
	connection := &toolsConnection{maximum: 1 << 20, pages: pages, required: required}
	t.Cleanup(func() { _ = connection.Close(ctx) })
	_, err = connection.Connect(ctx, clientPipe)
	return &toolService{connection: connection, members: map[string]*toolMember{method.Name(): member}}, err
}

func catalogInvocation(t *testing.T, method *portable.Method) portable.Invocation {
	t.Helper()
	input, err := value.ParseJSON([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var invocation portable.Invocation
	if method.Kind() == portable.Command {
		invocation, err = portable.NewCommandInvocation("original-command", method.Name(), input)
	} else {
		invocation, err = portable.NewQueryInvocation(method.Name(), input)
	}
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}

func TestToolsRejectImplicitCatalogRefreshAfterBinding(t *testing.T) {
	for _, kind := range []portable.MethodKind{portable.Query, portable.Command} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			method, tool := catalogContract(t, kind)
			var calls, refreshPages atomic.Int32
			server := sdk.NewServer(&sdk.Implementation{Name: "catalog-refresh", Version: "1"}, nil)
			server.AddTool(tool, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				calls.Add(1)
				return nil, &jsonrpc.Error{Code: sdk.CodeHeaderMismatch, Message: "original header mismatch"}
			})
			server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
				return func(ctx context.Context, name string, request sdk.Request) (sdk.Result, error) {
					if name == listToolsMethod && calls.Load() != 0 {
						page := refreshPages.Add(1)
						if page <= 3 {
							return &sdk.ListToolsResult{NextCursor: fmt.Sprint(page)}, nil
						}
						changed := *tool
						changed.InputSchema = json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`)
						return &sdk.ListToolsResult{Tools: []*sdk.Tool{&changed}}, nil
					}
					return next(ctx, name, request)
				}
			})
			service, err := connectCatalogOverPipe(t, ctx, server, method, 1)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Invoke(ctx, catalogInvocation(t, method))
			var mismatch *jsonrpc.Error
			if !errors.Is(err, process.ErrOutcomeUnknown) || !errors.As(err, &mismatch) || mismatch.Code != sdk.CodeHeaderMismatch {
				t.Fatalf("original uncertain MCP failure was replaced: %v", err)
			}
			if pages, invocations := refreshPages.Load(), calls.Load(); pages != 0 || invocations != 1 {
				t.Fatalf("frozen binding reopened discovery or replayed invocation: refreshPages=%d calls=%d", pages, invocations)
			}
			t.Logf("SDK protocol=%s refreshPages=%d calls=%d", service.connection.session.InitializeResult().ProtocolVersion, refreshPages.Load(), calls.Load())
		})
	}
}

func TestToolsDiscoveryStillHonorsCatalogBudget(t *testing.T) {
	for _, kind := range []portable.MethodKind{portable.Query, portable.Command} {
		for _, test := range []struct {
			name        string
			budget      int
			emptyPages  int
			repeat      bool
			servedPages int32
			want        error
		}{
			{name: "exact-budget", budget: 3, emptyPages: 2, servedPages: 3},
			{name: "empty-pages-exceed-budget", budget: 2, emptyPages: 2, servedPages: 2, want: frame.ErrCapacity},
			{name: "repeated-cursor", budget: 3, emptyPages: 2, repeat: true, servedPages: 2, want: frame.ErrContractMismatch},
		} {
			t.Run(string(kind)+"/"+test.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				method, tool := catalogContract(t, kind)
				var pages, calls atomic.Int32
				server := sdk.NewServer(&sdk.Implementation{Name: "bounded-catalog", Version: "1"}, nil)
				server.AddTool(tool, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					calls.Add(1)
					return &sdk.CallToolResult{StructuredContent: json.RawMessage(`{"accepted":true}`)}, nil
				})
				server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
					return func(ctx context.Context, name string, request sdk.Request) (sdk.Result, error) {
						if name == listToolsMethod {
							page := pages.Add(1)
							if page <= int32(test.emptyPages) {
								cursor := fmt.Sprint(page)
								if test.repeat {
									cursor = "repeated-cursor"
								}
								return &sdk.ListToolsResult{NextCursor: cursor}, nil
							}
							return &sdk.ListToolsResult{Tools: []*sdk.Tool{tool}}, nil
						}
						return next(ctx, name, request)
					}
				})
				service, err := connectCatalogOverPipe(t, ctx, server, method, test.budget)
				if !errors.Is(err, test.want) || pages.Load() != test.servedPages {
					t.Fatalf("catalog admission: pages=%d error=%v; want pages=%d error=%v", pages.Load(), err, test.servedPages, test.want)
				}
				if test.want != nil {
					if calls.Load() != 0 {
						t.Fatal("refused catalog dispatched a tool")
					}
					return
				}
				result, err := service.Invoke(ctx, catalogInvocation(t, method))
				if err != nil || result.String() != `{"accepted":true}` || calls.Load() != 1 || pages.Load() != test.servedPages {
					t.Fatalf("admitted tool: result=%s error=%v calls=%d pages=%d", result, err, calls.Load(), pages.Load())
				}
			})
		}
	}
}
