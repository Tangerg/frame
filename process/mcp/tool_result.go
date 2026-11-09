package mcp

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"sync"

	"github.com/Tangerg/frame/value"
	"github.com/Tangerg/go-sdk/jsonrpc"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

const callToolMethod = "tools/call"

type toolResultProjectionKey struct{}

// Only the adapter's stdio client uses this projection. The public Transport
// retains the SDK connection's server-side protocol negotiation capabilities.
type toolResultTransport struct{ source *Transport }

func (t toolResultTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	connection, err := t.source.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &resultConnection{Connection: connection}, nil
}

// The SDK alone assigns request IDs and completes calls. This projection keeps
// the first response's original JSON because CallToolResult discards extensions
// and normalizes absent content. It cannot dispatch, retry, cancel or settle a call.
type toolResultProjection struct {
	mu         sync.Mutex
	connection *resultConnection
	id         jsonrpc.ID
	raw        json.RawMessage
}

func (p *toolResultProjection) release() {
	p.mu.Lock()
	connection, id := p.connection, p.id
	p.mu.Unlock()
	if connection != nil {
		connection.mu.Lock()
		delete(connection.projections, id)
		connection.mu.Unlock()
	}
}

type resultConnection struct {
	sdk.Connection
	mu          sync.Mutex
	projections map[jsonrpc.ID]*toolResultProjection
}

func (c *resultConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	request, isRequest := message.(*jsonrpc.Request)
	projection, _ := ctx.Value(toolResultProjectionKey{}).(*toolResultProjection)
	if isRequest && request.IsCall() && request.Method == callToolMethod && projection != nil {
		c.mu.Lock()
		if c.projections == nil {
			c.projections = make(map[jsonrpc.ID]*toolResultProjection)
		}
		projection.mu.Lock()
		projection.connection, projection.id = c, request.ID
		projection.mu.Unlock()
		c.projections[request.ID] = projection
		c.mu.Unlock()
	}
	return c.Connection.Write(ctx, message)
}

func (c *resultConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	message, err := c.Connection.Read(ctx)
	if response, ok := message.(*jsonrpc.Response); err == nil && ok {
		c.mu.Lock()
		projection := c.projections[response.ID]
		delete(c.projections, response.ID)
		c.mu.Unlock()
		if projection != nil {
			projection.mu.Lock()
			projection.raw = append(json.RawMessage(nil), response.Result...)
			projection.mu.Unlock()
		}
	}
	return message, err
}

func (c *resultConnection) DropResponse(id jsonrpc.ID) {
	if dropper, ok := c.Connection.(interface{ DropResponse(jsonrpc.ID) }); ok {
		dropper.DropResponse(id)
	}
}

func (p *toolResultProjection) admit() (value.JSON, error) {
	p.mu.Lock()
	raw := p.raw
	p.mu.Unlock()
	result, err := value.ParseJSON(raw)
	if err != nil {
		return value.JSON{}, err
	}
	var envelope struct {
		Content json.RawMessage `json:"content"`
		IsError json.RawMessage `json:"isError"`
		Meta    json.RawMessage `json:"_meta"`
	}
	if result.String()[0] != '{' || jsonv2.Unmarshal(result.Bytes(), &envelope) != nil || len(envelope.Content) == 0 || envelope.Content[0] != '[' {
		return value.JSON{}, errors.New("mcp: tool result requires a content array")
	}
	if len(envelope.IsError) != 0 && string(envelope.IsError) != "true" && string(envelope.IsError) != "false" {
		return value.JSON{}, errors.New("mcp: tool result isError must be a boolean")
	}
	if len(envelope.Meta) != 0 && envelope.Meta[0] != '{' {
		return value.JSON{}, errors.New("mcp: tool result metadata must be an object")
	}
	return result, nil
}
