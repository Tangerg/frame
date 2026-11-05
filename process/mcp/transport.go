// Package mcp binds explicitly declared Frame services to MCP peers on the
// process driver's attached stream. Neither discovery nor a peer chooses grants.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"sync"

	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/internal/nilness"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/go-sdk/jsonrpc"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func connect(ctx context.Context, channel io.ReadWriteCloser, maximum int, configure func(*sdk.Client) error) (*sdk.ClientSession, error) {
	transport, err := NewTransport(channel, maximum)
	if err != nil {
		return nil, err
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "frame", Version: "1"}, &sdk.ClientOptions{
		Capabilities:   &sdk.ClientCapabilities{},
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})
	if configure != nil {
		if err := configure(client); err != nil {
			return nil, err
		}
	}
	return client.Connect(ctx, transport, nil)
}

// Transport carries one MCP session over the stream a process driver attached
// for one execution, under this package's wire profile. Each inbound frame is
// admitted whole, terminator included, within the frame budget and as exactly
// one strict JSON value before the SDK interprets it; each outbound frame stays
// within the same budget. Canceling a queued write leaves the active sender and
// readers alone. Canceling a write whose bytes are moving closes the stream,
// because the peer may already hold part of the frame.
//
// A custom [process.Protocol] connects its own SDK client through Transport in
// [process.Connection.Connect], so its methods and handlers get the same
// admission as [Tools] and [ManagedProtocol]. Transport neither launches nor
// releases the process: closing it closes the attached stream, and the driver
// alone establishes exit.
type Transport struct {
	channel io.ReadWriteCloser
	maximum int
}

// NewTransport binds the attached channel to the frame budget the driver passed
// to [process.Protocol.Prepare].
func NewTransport(channel io.ReadWriteCloser, maxFrameBytes int) (*Transport, error) {
	if nilness.Is(channel) || maxFrameBytes <= 0 {
		return nil, fmt.Errorf("%w: transport requires an attached channel and a positive frame budget", frame.ErrInvalid)
	}
	return &Transport{channel: channel, maximum: maxFrameBytes}, nil
}

const sdkFrameLimitDisabled = -1

// Connect implements the SDK transport contract for one session.
func (t *Transport) Connect(ctx context.Context) (sdk.Connection, error) {
	writer := &frameWriter{channel: t.channel, maximum: t.maximum}
	transport := &sdk.IOTransport{Reader: newFrameReader(t.channel, t.maximum), Writer: writer, MaxLineLength: sdkFrameLimitDisabled}
	conn, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &interruptibleConnection{Connection: conn, writer: writer, sending: make(chan struct{}, 1)}, nil
}

// Admit the complete newline-delimited frame before the SDK can decode a prefix.
// The adapter owns byte admission; the SDK alone owns JSON-RPC interpretation.
type frameReader struct {
	mu      sync.Mutex
	channel io.ReadCloser
	scanner *bufio.Scanner
	maximum int
	pending []byte
	err     error
}

func newFrameReader(channel io.ReadCloser, maximum int) *frameReader {
	reader := &frameReader{channel: channel, scanner: bufio.NewScanner(channel), maximum: maximum}
	reader.scanner.Buffer(make([]byte, min(maximum, bufio.MaxScanTokenSize)), maximum)
	reader.scanner.Split(reader.splitFrame)
	return reader
}

func (r *frameReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 && r.err == nil {
		r.pending, r.err = r.readFrame()
	}
	if r.err != nil {
		r.pending, r.scanner = nil, nil
		return 0, r.err
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	if len(r.pending) == 0 {
		r.pending = nil
	}
	return n, nil
}

func (r *frameReader) readFrame() ([]byte, error) {
	if r.scanner.Scan() {
		return r.scanner.Bytes(), nil
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (r *frameReader) splitFrame(data []byte, atEOF bool) (int, []byte, error) {
	if end := bytes.IndexByte(data, '\n'); end >= 0 {
		size := end + 1
		if size > r.maximum {
			return 0, nil, frameSizeError(r.maximum)
		}
		// Reject ambiguous names and malformed Unicode before SDK decoding can
		// replace their original wire representation. Each line owns one value.
		if !jsontext.Value(data[:size]).IsValid() {
			return 0, nil, fmt.Errorf("%w: MCP frame must contain one complete JSON value", frame.ErrContractMismatch)
		}
		return size, data[:size], nil
	}
	if len(data) >= r.maximum {
		return 0, nil, frameSizeError(r.maximum)
	}
	if atEOF && len(data) > 0 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return 0, nil, nil
}

func (r *frameReader) Close() error {
	// Interrupt the blocked read before joining its access to the frame buffer.
	err := r.channel.Close()
	r.mu.Lock()
	r.pending, r.scanner, r.err = nil, nil, io.EOF
	r.mu.Unlock()
	return err
}

func frameSizeError(maximum int) error {
	return fmt.Errorf("%w: MCP frame exceeds %d bytes", frame.ErrCapacity, maximum)
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
		return cancellation.Err(ctx)
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
		return 0, frameSizeError(w.maximum)
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
