package runner

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

type attachment struct {
	stdin     io.Writer
	stdout    io.ReadCloser
	closeOnce sync.Once
	closeErr  error
}

func (a *attachment) Close() error {
	a.closeOnce.Do(func() { a.closeErr = a.stdout.Close() })
	return a.closeErr
}

func (s *Server) serveAttachment(ctx context.Context, connection net.Conn, reader io.Reader, ref AttemptRef, release func()) {
	attached, err := s.attach(ref)
	if err != nil {
		_ = s.writeResponse(connection, nil, err)
		return
	}
	defer attached.Close()
	stop := context.AfterFunc(ctx, func() { _ = attached.Close() })
	defer stop()
	if err := s.writeResponse(connection, nil, nil); err != nil {
		_, _ = s.CloseAttempt(context.Background(), ref)
		return
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		_, _ = s.CloseAttempt(context.Background(), ref)
		return
	}
	release()
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		_, _ = io.Copy(attached.stdin, reader)
		_, _ = s.CloseAttempt(context.Background(), ref)
	}()
	_, _ = io.Copy(connection, attached.stdout)
	_ = attached.Close()
	_ = connection.Close()
	<-inputDone
}
