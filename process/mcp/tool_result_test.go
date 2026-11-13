package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
	"github.com/Tangerg/go-sdk/jsonrpc"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func toolWireService(t *testing.T, result string, fullResult bool, outputSchema string) *toolService {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	go func() {
		scanner := bufio.NewScanner(peer)
		for scanner.Scan() {
			var request struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				return
			}
			if len(request.ID) == 0 {
				continue
			}
			switch request.Method {
			case "server/discover":
				_, _ = fmt.Fprintf(peer, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32601,\"message\":\"unsupported\"}}\n", request.ID)
			case "initialize":
				_, _ = fmt.Fprintf(peer, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"raw-tool\",\"version\":\"1\"}}}\n", request.ID)
			case callToolMethod:
				_, _ = fmt.Fprintf(peer, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n", request.ID, result)
			}
		}
	}()
	session, err := connect(ctx, client, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	method, err := frame.AdmitMethod(frame.MethodDescriptor{Name: "write", Kind: frame.Command, Input: []byte(`{"type":"object","additionalProperties":false}`), Output: []byte(outputSchema)})
	if err != nil {
		t.Fatal(err)
	}
	return &toolService{connection: &toolsConnection{session: session}, members: map[string]*toolMember{"write": {method: method, fullResult: fullResult}}}
}

func TestToolResultRequiresOriginalContentArray(t *testing.T) {
	for _, result := range []string{"null", `{}`, `{"content":null}`, `{"content":{}}`, `{"Content":[]}`, `{"content":[],"isError":null}`, `{"content":[],"_meta":null}`} {
		t.Run(result, func(t *testing.T) {
			service := toolWireService(t, result, true, `{"type":"object","required":["content"]}`)
			input, _ := value.ParseJSON([]byte(`{}`))
			_, err := service.Invoke(t.Context(), commandInvocation(t, "original-command", "write", input))
			if !errors.Is(err, frame.ErrOutcomeUnknown) {
				t.Fatalf("invalid original result became success: %v", err)
			}
		})
	}
}

func TestToolResultExtensionsCannotChangeOutcomeClassification(t *testing.T) {
	const result = `{"content":[],"isError":false,"IsError":true,"ResultType":"input_required"}`
	service := toolWireService(t, result, true, `{"type":"object","required":["content"]}`)
	input, _ := value.ParseJSON([]byte(`{}`))
	reply, err := service.Invoke(t.Context(), commandInvocation(t, "original-command", "write", input))
	want, _ := value.ParseJSON([]byte(result))
	if err != nil || reply != want {
		t.Fatalf("extension changed result classification: %s %v", reply.String(), err)
	}
}

func TestToolResultAdmitsNegotiatedCompleteEnvelope(t *testing.T) {
	method, err := frame.AdmitMethod(frame.MethodDescriptor{
		Name: "read", Kind: frame.Query, Input: []byte(`{"type":"object","additionalProperties":false}`),
		Output: []byte(`{"type":"object","required":["content","resultType"],"properties":{"resultType":{"const":"complete"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "full-result-version", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{
		Name: method.Name(), InputSchema: json.RawMessage(method.Descriptor().Input), OutputSchema: json.RawMessage(method.Descriptor().Output),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "result"}}}, nil
	})
	service, err := connectCatalogOverPipe(t, t.Context(), server, method, 1)
	if err != nil {
		t.Fatal(err)
	}
	service.members[method.Name()].fullResult = true
	reply, err := service.Invoke(t.Context(), catalogInvocation(t, method))
	if err != nil {
		t.Fatalf("negotiated result was rejected: %s %v", reply.String(), err)
	}
	t.Logf("negotiated MCP protocol=%s", service.connection.session.InitializeResult().ProtocolVersion)
}

func TestToolResultRetainsEnvelopeExtensions(t *testing.T) {
	const result = `{"content":[{"type":"text","text":"ok","vendor":{"exact":9007199254740993}}],"runId":"original-run","isError":false}`
	service := toolWireService(t, result, true, `{"type":"object","required":["content","runId"],"properties":{"content":{"type":"array"},"runId":{"type":"string"}}}`)
	input, _ := value.ParseJSON([]byte(`{}`))
	reply, err := service.Invoke(t.Context(), commandInvocation(t, "original-command", "write", input))
	want, _ := value.ParseJSON([]byte(result))
	if err != nil || reply != want {
		t.Fatalf("full result changed original fields: %s %v", reply.String(), err)
	}
}

func TestToolErrorRetainsOriginalEnvelope(t *testing.T) {
	const result = `{"content":[],"isError":true,"runId":"refused-run"}`
	service := toolWireService(t, result, false, `{"type":"string"}`)
	input, _ := value.ParseJSON([]byte(`{}`))
	_, err := service.Invoke(t.Context(), commandInvocation(t, "original-command", "write", input))
	reported, ok := errors.AsType[*ToolError](err)
	want, _ := value.ParseJSON([]byte(result))
	if !ok || reported.Result != want {
		t.Fatalf("tool error lost original result: %v", err)
	}
}

type projectionPeer struct {
	reads  chan jsonrpc.Message
	writes chan jsonrpc.Message
}

func (p *projectionPeer) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case message := <-p.reads:
		return message, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (p *projectionPeer) Write(_ context.Context, message jsonrpc.Message) error {
	p.writes <- message
	return nil
}
func (*projectionPeer) Close() error      { return nil }
func (*projectionPeer) SessionID() string { return "" }

func TestToolProjectionFollowsOriginalResponseIdentity(t *testing.T) {
	peer := &projectionPeer{reads: make(chan jsonrpc.Message, 3), writes: make(chan jsonrpc.Message, 2)}
	connection := &resultConnection{Connection: peer}
	first, second := new(toolResultProjection), new(toolResultProjection)
	defer first.release()
	defer second.release()
	firstID, _ := jsonrpc.MakeID("first")
	secondID, _ := jsonrpc.MakeID("second")
	var writes sync.WaitGroup
	for _, request := range []struct {
		id         jsonrpc.ID
		projection *toolResultProjection
	}{{firstID, first}, {secondID, second}} {
		writes.Go(func() {
			message := &jsonrpc.Request{ID: request.id, Method: callToolMethod}
			if err := connection.Write(context.WithValue(t.Context(), toolResultProjectionKey{}, request.projection), message); err != nil {
				t.Error(err)
			}
		})
	}
	writes.Wait()
	firstResult, secondResult := json.RawMessage(`{"content":[],"runId":"first"}`), json.RawMessage(`{"content":[],"runId":"second"}`)
	for _, response := range []*jsonrpc.Response{
		{ID: secondID, Result: secondResult},
		{ID: firstID, Result: firstResult},
		{ID: firstID, Result: secondResult},
	} {
		peer.reads <- response
		if observed, err := connection.Read(t.Context()); err != nil || observed != response {
			t.Fatalf("projection changed SDK message: %v %v", observed, err)
		}
	}
	for _, check := range []struct {
		projection *toolResultProjection
		want       json.RawMessage
	}{{first, firstResult}, {second, secondResult}} {
		got, err := check.projection.admit()
		want, _ := value.ParseJSON(check.want)
		if err != nil || got != want {
			t.Fatalf("projection chose another response: %s %v", got.String(), err)
		}
	}
	if len(connection.projections) != 0 {
		t.Fatal("observed responses retained a second request lifecycle")
	}
}

func TestToolProjectionRetiresWithItsSDKCall(t *testing.T) {
	peer := &projectionPeer{reads: make(chan jsonrpc.Message, 1), writes: make(chan jsonrpc.Message, 1)}
	connection := &resultConnection{Connection: peer}
	projection := new(toolResultProjection)
	id, _ := jsonrpc.MakeID("cancelled")
	if err := connection.Write(context.WithValue(t.Context(), toolResultProjectionKey{}, projection), &jsonrpc.Request{ID: id, Method: callToolMethod}); err != nil {
		t.Fatal(err)
	}
	projection.release()
	if len(connection.projections) != 0 {
		t.Fatal("returned SDK call retained a pending projection")
	}
	peer.reads <- &jsonrpc.Response{ID: id, Result: json.RawMessage(`{"content":[]}`)}
	if _, err := connection.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := projection.admit(); err == nil {
		t.Fatal("late response revived a retired call")
	}
}

var _ sdk.Connection = (*projectionPeer)(nil)
