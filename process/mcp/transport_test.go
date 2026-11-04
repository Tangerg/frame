package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/go-sdk/jsonrpc"
)

type inputStream struct{ io.Reader }

func (inputStream) Write(data []byte) (int, error) { return len(data), nil }
func (inputStream) Close() error                   { return nil }

func TestInboundFrameBudgetIsIndependentOfEarlierFramesAndReadBoundaries(t *testing.T) {
	const maximum = 256
	small := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	message := func(size int) string {
		prefix := `{"jsonrpc":"2.0","id":2,"method":"ping","params":{"value":"`
		suffix := `"}}` + "\n"
		return prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
	}
	for _, test := range []struct {
		name     string
		input    string
		accepted int
		failure  error
	}{
		{name: "exact budget", input: message(maximum), accepted: 1, failure: io.EOF},
		{name: "consecutive exact budgets", input: message(maximum) + message(maximum), accepted: 2, failure: io.EOF},
		{name: "exact numeric lexeme", input: `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"value":9.007199254740993e15}}` + "\n", accepted: 1, failure: io.EOF},
		{name: "paired surrogate", input: `{"jsonrpc":"2.0","id":1,"method":"\ud834\udd1e"}` + "\n", accepted: 1, failure: io.EOF},
		{name: "oversized alone", input: message(maximum + 1), failure: frame.ErrCapacity},
		{name: "small then oversized", input: small + message(maximum+21), accepted: 1, failure: frame.ErrCapacity},
		{name: "oversized whitespace suffix", input: strings.TrimSuffix(small, "\n") + strings.Repeat(" ", maximum) + "\n", failure: frame.ErrCapacity},
		{name: "unterminated frame", input: strings.TrimSuffix(small, "\n"), failure: io.ErrUnexpectedEOF},
		{name: "oversized unterminated frame", input: strings.TrimSuffix(message(maximum+2), "\n"), failure: frame.ErrCapacity},
		{name: "multiline value", input: "{\n" + strings.TrimPrefix(small, "{"), failure: frame.ErrContractMismatch},
		{name: "multiline whitespace cannot extend a value", input: "{\n" + strings.Repeat(" \n", maximum) + strings.TrimPrefix(small, "{"), failure: frame.ErrContractMismatch},
		{name: "blank frames cannot aggregate", input: strings.Repeat(" \n", maximum) + small, failure: frame.ErrContractMismatch},
		{name: "one line cannot contain multiple values", input: "{}{}\n", failure: frame.ErrContractMismatch},
		{name: "invalid UTF-8", input: "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"\xff\"}\n", failure: frame.ErrContractMismatch},
		{name: "duplicate message identity", input: `{"jsonrpc":"2.0","id":1,"id":2,"method":"ping"}` + "\n", failure: frame.ErrContractMismatch},
		{name: "duplicate schema member", input: `{"jsonrpc":"2.0","id":1,"result":{"inputSchema":{"type":"number","type":"string"}}}` + "\n", failure: frame.ErrContractMismatch},
		{name: "unpaired surrogate", input: `{"jsonrpc":"2.0","id":1,"method":"\ud800"}` + "\n", failure: frame.ErrContractMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, chunks := range []struct {
				name string
				read func(io.Reader) io.Reader
			}{
				{name: "together", read: func(r io.Reader) io.Reader { return r }},
				{name: "one byte", read: iotest.OneByteReader},
				{name: "short reads", read: iotest.HalfReader},
			} {
				t.Run(chunks.name, func(t *testing.T) {
					channel := inputStream{chunks.read(strings.NewReader(test.input))}
					connection, err := (attachedTransport{channel: channel, maximum: maximum}).Connect(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer connection.Close()
					for index := range test.accepted {
						if _, err := connection.Read(t.Context()); err != nil {
							t.Fatalf("accepted frame %d: %v", index, err)
						}
					}
					if _, err := connection.Read(t.Context()); !errors.Is(err, test.failure) {
						t.Fatalf("frame admission: got %v, want %v", err, test.failure)
					}
				})
			}
		})
	}
}

func TestUnterminatedInboundFrameFailsAtBudgetWithoutWaitingForEOF(t *testing.T) {
	const maximum = 256
	from, to := net.Pipe()
	defer to.Close()
	connection, err := (attachedTransport{channel: from, maximum: maximum}).Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(to, strings.Repeat(" ", maximum)); written <- err }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := connection.Read(ctx); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("unterminated frame admission: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

type blockedWriter struct {
	net.Conn
	entered chan struct{}
}

func (w blockedWriter) Write(p []byte) (int, error) {
	close(w.entered)
	return w.Conn.Write(p)
}

func TestCanceledBlockedWriteReleasesStream(t *testing.T) {
	from, to := net.Pipe()
	defer to.Close()
	writer := blockedWriter{from, make(chan struct{})}
	transport := attachedTransport{writer, 1 << 10}
	conn, err := transport.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	message, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- conn.Write(ctx, message) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("write never began")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("blocked write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled write remains blocked")
	}
}

// Observing Done synchronizes the test with entry to the write queue.
type queuedContext struct {
	context.Context
	observe func()
}

func (q queuedContext) Done() <-chan struct{} { q.observe(); return q.Context.Done() }

func TestCancelQueuedWriteKeepsActiveSenderAndReader(t *testing.T) {
	from, to := net.Pipe()
	defer to.Close()
	writer := blockedWriter{from, make(chan struct{})}
	conn, err := (attachedTransport{writer, 1 << 10}).Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	message, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- conn.Write(t.Context(), message) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("first write never began")
	}
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	queued := make(chan struct{})
	ctx := queuedContext{base, sync.OnceFunc(func() { close(queued) })}
	second := make(chan error, 1)
	go func() { second <- conn.Write(ctx, message) }()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("second writer never entered queue")
	}
	cancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued cancellation waited for the active writer")
	}
	if err := to.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(to).ReadBytes('\n'); err != nil {
		t.Fatalf("active frame lost: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("active write: %v", err)
	}
	read := make(chan error, 1)
	go func() { _, err := conn.Read(t.Context()); read <- err }()
	if _, err := io.WriteString(to, `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n"); err != nil {
		t.Fatalf("response stream closed: %v", err)
	}
	if err := <-read; err != nil {
		t.Fatalf("other call lost its response: %v", err)
	}
}
