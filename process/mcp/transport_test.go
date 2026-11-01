package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/go-sdk/jsonrpc"
)

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
