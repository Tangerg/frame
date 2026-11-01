// Package mcp binds explicitly declared Frame services to MCP peers on the
// process driver's attached stream. Neither discovery nor a peer chooses grants.
package mcp

import (
	"context"
	"fmt"
	"io"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/go-sdk/jsonrpc"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func connect(ctx context.Context, channel io.ReadWriteCloser, maximum int, configure func(*sdk.Client) error) (*sdk.ClientSession, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: "frame", Version: "1"}, &sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}})
	if configure != nil {
		if err := configure(client); err != nil {
			return nil, err
		}
	}
	return client.Connect(ctx, attachedTransport{channel, maximum}, nil)
}

type attachedTransport struct {
	channel io.ReadWriteCloser
	maximum int
}

func (t attachedTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	writer := &frameWriter{channel: t.channel, maximum: t.maximum}
	transport := &sdk.IOTransport{Reader: t.channel, Writer: writer, MaxLineLength: t.maximum}
	conn, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &interruptibleConnection{Connection: conn, writer: writer, sending: make(chan struct{}, 1)}, nil
}

type interruptibleConnection struct {
	sdk.Connection
	writer  *frameWriter
	sending chan struct{}
}

func (c *interruptibleConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	// The SDK serializes writes behind a mutex. A waiting caller must be able
	// to leave that queue without interrupting the sender that owns the stream.
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case c.sending <- struct{}{}:
	}
	defer func() {
		c.writer.ctx = nil
		<-c.sending
	}()
	c.writer.ctx = ctx
	return c.Connection.Write(ctx, message)
}

// The SDK issues one synchronous Write per complete JSON-RPC frame. Only the
// caller actually writing bytes may interrupt this shared stream. If the child
// stops reading, that write may already have sent a partial frame.
type frameWriter struct {
	channel io.ReadWriteCloser
	maximum int
	ctx     context.Context
}

func (w *frameWriter) Write(frameBytes []byte) (int, error) {
	if len(frameBytes) > w.maximum {
		return 0, fmt.Errorf("%w: MCP frame exceeds %d bytes", frame.ErrCapacity, w.maximum)
	}
	if err := context.Cause(w.ctx); err != nil {
		return 0, err
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(w.ctx, func() {
		_ = w.channel.Close()
		close(closed)
	})
	n, err := w.channel.Write(frameBytes)
	if !stop() {
		// Keep the write slot until cancellation has finished interrupting it.
		<-closed
	}
	if err == nil && n != len(frameBytes) {
		err = io.ErrShortWrite
	}
	return n, err
}

// A duplex stream has a single descriptor. The SDK closes it via its read half.
func (*frameWriter) Close() error { return nil }
