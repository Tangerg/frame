package runner

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Tangerg/frame"
)

type attachment struct {
	stdin   io.Writer
	stdout  io.ReadCloser
	control net.Conn
	mu      sync.Mutex
	stream  net.Conn
	closed  bool
	err     error
}

func (a *attachment) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closeLocked()
}

func (a *attachment) closeLocked() error {
	if !a.closed {
		a.closed = true
		a.err = errors.Join(a.control.Close(), a.stdout.Close())
		if a.stream != nil {
			a.err = errors.Join(a.err, a.stream.Close())
		}
	}
	return a.err
}

func (a *attachment) bindStream(connection net.Conn) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return frame.ErrClosed
	}
	if a.stream != nil {
		return ErrAttached
	}
	a.stream = connection
	return nil
}

func (a *attachment) closeUnconnected() {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A connected reader owns draining the child's final output before closing.
	if a.stream == nil {
		_ = a.closeLocked()
	}
}

func (s *Server) serveAttachmentControl(ctx context.Context, connection net.Conn, reader io.Reader, ref AttemptRef, release func()) {
	attached, err := s.attach(ref, connection)
	if err != nil {
		_ = s.writeResponse(connection, nil, err)
		return
	}
	defer func() {
		_ = attached.Close()
		_, _ = s.CloseAttempt(context.Background(), ref)
	}()
	stop := context.AfterFunc(ctx, func() { _ = attached.Close() })
	defer stop()
	if err := s.writeResponse(connection, nil, nil); err != nil {
		return
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return
	}
	release()
	// Stdin backpressure can hide data-stream EOF. This connection carries no
	// payload, so losing the host always reaches the original attempt's close path.
	var unexpected [1]byte
	_, _ = io.ReadFull(reader, unexpected[:])
}

func (s *Server) serveAttachmentStream(ctx context.Context, connection net.Conn, reader io.Reader, ref AttemptRef, release func()) {
	attached, err := s.attachStream(ref, connection)
	if err != nil {
		_ = s.writeResponse(connection, nil, err)
		return
	}
	defer attached.Close()
	stop := context.AfterFunc(ctx, func() { _ = attached.Close() })
	defer stop()
	if err := s.writeResponse(connection, nil, nil); err != nil {
		return
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return
	}
	release()
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		_, _ = io.Copy(attached.stdin, reader)
		_ = attached.Close()
	}()
	_, _ = io.Copy(connection, attached.stdout)
	_ = attached.Close()
	// Closing control advances CloseAttempt independently of a blocked stdin
	// writer. Its original owner closes stdin before this join can complete.
	<-inputDone
}
