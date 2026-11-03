package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

const inputSchema = `{"type":"object","additionalProperties":false,"properties":{"action":{"type":"string"}}}`
const outputSchema = `{"type":"object","required":["n"],"properties":{"n":{"type":"integer"}}}`
const exactResult = `{"n":9007199254740993}`

// Invoked as a separate executable by the real Frame runner.
func TestStdioPeer(t *testing.T) {
	dir := os.Getenv("FRAME_MCP_TOOLS_FIXTURE")
	if dir == "" {
		return
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "third-party", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	schema := outputSchema
	if os.Getenv("FRAME_MCP_SCHEMA_DRIFT") == "1" {
		schema = `{"type":"object"}`
	}
	server.AddTool(&sdk.Tool{Name: "read", InputSchema: json.RawMessage(inputSchema), OutputSchema: json.RawMessage(schema), Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var args struct{ Action string }
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			switch args.Action {
			case "fail":
				return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "tool declined"}}}, nil
			case "invalid":
				return &sdk.CallToolResult{StructuredContent: map[string]string{"n": "bad"}}, nil
			case "wait":
				if err := os.WriteFile(filepath.Join(dir, "entered"), nil, 0600); err != nil {
					return nil, err
				}
				<-ctx.Done()
				if err := os.WriteFile(filepath.Join(dir, "canceled"), nil, 0600); err != nil {
					return nil, err
				}
				return nil, ctx.Err()
			default:
				return &sdk.CallToolResult{StructuredContent: json.RawMessage(exactResult)}, nil
			}
		})
	for _, name := range []string{"text", "secret"} {
		server.AddTool(&sdk.Tool{Name: name, InputSchema: json.RawMessage(inputSchema)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			if name == "secret" {
				_ = os.WriteFile(filepath.Join(dir, "leaked"), nil, 0600)
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name}}}, nil
		})
	}
	if err := server.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestToolsOnApprovedRunner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages int
		drift bool
		want  error
	}{
		{"call", 4, false, nil}, {"catalog-budget", 1, false, frame.ErrCapacity}, {"schema-drift", 4, true, frame.ErrContractMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capability, err := portable.NewCapability("thirdparty", portable.MethodDescriptor{Name: "read", Kind: portable.Query, Input: []byte(inputSchema), Output: []byte(outputSchema)}, portable.MethodDescriptor{Name: "text", Kind: portable.Command, Input: []byte(inputSchema), Output: []byte(`{"type":"object","required":["content"]}`)})
			if err != nil {
				t.Fatal(err)
			}
			p := Tools{MaxCatalogPages: tc.pages, FullResult: func(_ frame.ExportDeclaration, name string) bool { return name == "text" }}
			checkExecution(t, p, capability, func(t *testing.T, dir string) process.Program {
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				binary, err := os.ReadFile(executable)
				if err != nil {
					t.Fatal(err)
				}
				env := append(os.Environ(), "FRAME_MCP_TOOLS_FIXTURE="+dir)
				if tc.drift {
					env = append(env, "FRAME_MCP_SCHEMA_DRIFT=1")
				}
				return process.Program{Command: executable, ExecutableDigest: value.Hash(binary), Env: env, Args: []string{"-test.run=^TestStdioPeer$"}, Directory: dir}
			}, tc.want, func(ctx context.Context, svc process.Service, dir string) { verifyCalls(t, ctx, svc, dir) })
		})
	}
}

func verifyCalls(t *testing.T, ctx context.Context, svc process.Service, dir string) {
	t.Helper()
	call := func(ctx context.Context, name, args string) (value.JSON, error) {
		in, err := value.ParseJSON([]byte(args))
		if err != nil {
			t.Fatal(err)
		}
		return svc.Invoke(ctx, queryInvocation(t, name, in))
	}
	reply, err := call(ctx, "read", "{}")
	if err != nil || reply.String() != exactResult {
		t.Fatalf("large integer: %s %v", reply, err)
	}
	input, err := value.ParseJSON([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	reply, err = svc.Invoke(ctx, commandInvocation(t, "original-text", "text", input))
	if err != nil || !strings.Contains(reply.String(), `"text":"text"`) {
		t.Fatalf("text result: %s %v", reply, err)
	}
	if _, err := call(ctx, "secret", "{}"); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("undeclared tool: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "leaked")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("undeclared tool ran: %v", err)
	}
	if _, err := call(ctx, "read", `{"unexpected":true}`); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("invalid input: %v", err)
	}
	var reported *ToolError
	if _, err := call(ctx, "read", `{"action":"fail"}`); !errors.As(err, &reported) || !strings.Contains(reported.Result.String(), "tool declined") {
		t.Fatalf("explicit tool failure: %v", err)
	}
	if _, err := call(ctx, "read", `{"action":"invalid"}`); !errors.Is(err, process.ErrOutcomeUnknown) {
		t.Fatalf("invalid output: %v", err)
	}
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := call(request, "read", `{"action":"wait"}`); done <- err }()
	awaitMarker(t, ctx, filepath.Join(dir, "entered"))
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, process.ErrOutcomeUnknown) {
		t.Fatalf("canceled dispatched call: %v", err)
	}
	awaitMarker(t, ctx, filepath.Join(dir, "canceled"))
}

func awaitMarker(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestToolCannotWeakenApprovedContract(t *testing.T) {
	method, err := portable.AdmitMethod(portable.MethodDescriptor{Name: "read", Kind: portable.Query, Input: []byte(inputSchema), Output: []byte(outputSchema)})
	if err != nil {
		t.Fatal(err)
	}
	member := toolMember{method: method}
	tool := sdk.Tool{Name: "read", InputSchema: json.RawMessage(inputSchema), OutputSchema: json.RawMessage(outputSchema)}
	if err := member.bind(&tool); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("missing read-only hint: %v", err)
	}
	tool.Annotations = &sdk.ToolAnnotations{ReadOnlyHint: true}
	tool.OutputSchema = nil
	if err := member.bind(&tool); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("downgraded structured output: %v", err)
	}
	member.fullResult = true
	if err := member.bind(&tool); err != nil {
		t.Fatalf("host-selected result envelope: %v", err)
	}
}
