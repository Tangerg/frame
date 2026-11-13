package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Tangerg/frame"
)

const (
	maxControlBytes           = 4 << 20
	controlReadBufferBytes    = 4096
	defaultControlConnections = 128
	defaultControlTimeout     = 10 * time.Second
)

type controlMethod string

const (
	methodIdentity        controlMethod = "identity"
	methodFence           controlMethod = "fence"
	methodProveAbsent     controlMethod = "prove_absent"
	methodAcknowledgeExit controlMethod = "acknowledge_exit"
	methodLaunch          controlMethod = "launch"
	methodInspect         controlMethod = "inspect"
	methodClose           controlMethod = "close"
	methodAttachControl   controlMethod = "attach_control"
	methodAttachStream    controlMethod = "attach_stream"
)

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	ctx, owned, err := s.admitServe(ctx, listener)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	slots := make(chan struct{}, s.config.MaxControlConnections)
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	defer func() {
		_ = listener.Close()
		workers.Wait()
		close(owned.served)
	}()
	for {
		select {
		case <-ctx.Done():
			return frame.ContextError(ctx)
		case slots <- struct{}{}:
		}
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return frame.ContextError(ctx)
			}
			return err
		}
		release := sync.OnceFunc(func() { <-slots })
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer release()
			defer connection.Close()
			stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
			defer stop()
			s.handle(ctx, connection, release)
		}()
	}
}

func (s *Server) handle(ctx context.Context, connection net.Conn, release func()) {
	if err := connection.SetReadDeadline(time.Now().Add(s.config.ControlTimeout)); err != nil {
		return
	}
	reader := bufio.NewReaderSize(connection, controlReadBufferBytes)
	request, err := readRequest(reader)
	if err != nil {
		_ = s.writeAcknowledgment(connection, err)
		return
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	switch request := request.(type) {
	case attachControlRequest:
		s.serveAttachmentControl(ctx, connection, reader, request.AttemptRef, release)
		return
	case attachStreamRequest:
		s.serveAttachmentStream(ctx, connection, reader, request.AttemptRef, release)
		return
	}
	response, err := s.respond(ctx, request)
	_ = s.writeReply(connection, response, err)
}

func (s *Server) respond(ctx context.Context, request controlRequest) (controlResult, error) {
	switch request := request.(type) {
	case identityRequest:
		identity, err := s.Identity(ctx)
		if err != nil {
			return nil, err
		}
		return identity, nil
	case fenceRequest:
		return acknowledged(s.Fence(ctx, request.Epoch))
	case proveAbsentRequest:
		proof, err := s.ProveAbsent(ctx, request.AttemptRef)
		if err != nil {
			return nil, err
		}
		return proof, nil
	case acknowledgeExitRequest:
		return acknowledged(s.AcknowledgeExit(ctx, request.ExitReceipt))
	case launchControlRequest:
		result, err := s.Launch(ctx, request.LaunchRequest)
		return observed(result.Observation, err)
	case inspectRequest:
		return observed(s.Inspect(ctx, request.AttemptID))
	case closeRequest:
		return observed(s.CloseAttempt(ctx, request.AttemptRef))
	default:
		return nil, frame.ErrInvalid
	}
}

// observed reports an observation even beside an error, so a failed close
// still tells the caller the attempt's current state.
func observed(observation Observation, err error) (controlResult, error) {
	if observation.AttemptID == "" {
		return nil, err
	}
	return observation, err
}

func (s *Server) attach(ref AttemptRef, control net.Conn) (*attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live, err := s.attachableProcess(ref)
	if err != nil {
		return nil, err
	}
	if live.attachment != nil {
		return nil, ErrAttached
	}
	attached := &attachment{stdin: live.stdin, stdout: live.stdout, control: control}
	live.stdout = nil
	live.attachment = attached
	return attached, nil
}

func (s *Server) attachStream(ref AttemptRef, connection net.Conn) (*attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live, err := s.attachableProcess(ref)
	if err != nil {
		return nil, err
	}
	if live.attachment == nil {
		return nil, fmt.Errorf("%w: attachment control is required", frame.ErrInvalid)
	}
	return live.attachment, live.attachment.bindStream(connection)
}

func (s *Server) attachableProcess(ref AttemptRef) (*process, error) {
	if s.closed {
		return nil, frame.ErrClosed
	}
	item, exists, err := s.lookupAttempt(ref.AttemptID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	if item.Observation.Reference() != ref {
		return nil, ErrFenced
	}
	if item.Observation.State == Unknown {
		return nil, ErrUnknown
	}
	live := s.processes[ref.AttemptID]
	if live == nil || live.closeState != processOpen {
		return nil, frame.ErrClosed
	}
	return live, nil
}

type Client struct{ socket string }

func NewClient(socket string) (*Client, error) {
	if err := validateSocketPath(socket); err != nil {
		return nil, err
	}
	return &Client{socket: socket}, nil
}

func validateSocketPath(socket string) error {
	limit := socketPathLimit()
	if limit == 0 {
		return fmt.Errorf("%w: runner Unix transport is unsupported on this platform", frame.ErrInvalid)
	}
	if !filepath.IsAbs(socket) || strings.ContainsRune(socket, '\x00') {
		return fmt.Errorf("%w: runner socket path must be absolute and contain no NUL", frame.ErrInvalid)
	}
	if len(socket) > limit {
		return fmt.Errorf("%w: runner socket path exceeds the platform limit of %d bytes", frame.ErrInvalid, limit)
	}
	return nil
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c == nil || c.socket == "" {
		return nil, frame.ErrInvalid
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(c.socket))
	if err != nil {
		return nil, err
	}
	if err := verifyAuthorityDirectory(directory); err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(directory, filepath.Base(c.socket)))
}

func (c *Client) Identity(ctx context.Context) (Identity, error) {
	response, _, err := c.request(ctx, identityRequest{})
	if err != nil {
		return Identity{}, err
	}
	return response.(Identity), nil
}

func (c *Client) Fence(ctx context.Context, epoch uint64) error {
	_, err := c.control(ctx, fenceRequest{Epoch: epoch})
	return err
}
func (c *Client) Launch(ctx context.Context, request LaunchRequest) (LaunchResult, error) {
	if err := request.Validate(); err != nil {
		return notDispatched(request), err
	}
	response, dispatch, err := c.request(ctx, launchControlRequest{request})
	if dispatch == controlNotDispatched {
		return notDispatched(request), err
	}
	observation, _ := response.(Observation)
	return LaunchResult{Observation: observation}, err
}
func (c *Client) Inspect(ctx context.Context, id string) (Observation, error) {
	return c.control(ctx, inspectRequest{AttemptID: id})
}
func (c *Client) CloseAttempt(ctx context.Context, ref AttemptRef) (Observation, error) {
	return c.control(ctx, closeRequest{ref})
}

func (c *Client) AcknowledgeExit(ctx context.Context, receipt ExitReceipt) error {
	_, err := c.control(ctx, acknowledgeExitRequest{receipt})
	return err
}

func (c *Client) ProveAbsent(ctx context.Context, request AttemptRef) (AbsenceProof, error) {
	response, _, err := c.request(ctx, proveAbsentRequest{request})
	if err != nil {
		return AbsenceProof{}, err
	}
	proof := response.(AbsenceProof)
	if proof.Validate(request) != nil {
		return AbsenceProof{}, ErrUnknown
	}
	return proof, nil
}

func (c *Client) control(ctx context.Context, request controlRequest) (Observation, error) {
	response, _, err := c.request(ctx, request)
	observation, _ := response.(Observation)
	return observation, err
}

type controlDispatch uint8

const (
	controlDispatchUnknown controlDispatch = iota
	controlNotDispatched
)

func (c *Client) request(ctx context.Context, request controlRequest) (controlResult, controlDispatch, error) {
	if err := frame.ContextError(ctx); err != nil {
		return nil, controlNotDispatched, err
	}
	data, err := encodeRequest(request)
	if err != nil {
		return nil, controlNotDispatched, err
	}
	connection, err := c.dial(ctx)
	if err != nil {
		return nil, controlNotDispatched, errors.Join(err, frame.ContextError(ctx))
	}
	defer connection.Close()
	return exchangeControl(ctx, connection, request.method(), data)
}

func exchangeControl(ctx context.Context, connection net.Conn, method controlMethod, data []byte) (controlResult, controlDispatch, error) {
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := frame.ContextError(ctx); err != nil {
		return nil, controlNotDispatched, err
	}
	// Once Write starts, a transport error cannot establish how much the original
	// runner accepted. Only that runner's observation can settle this invocation.
	if err := writeControl(connection, data); err != nil {
		return nil, controlDispatchUnknown, errors.Join(err, frame.ContextError(ctx))
	}
	line, err := readLine(bufio.NewReader(connection))
	if err != nil {
		return nil, controlDispatchUnknown, errors.Join(err, frame.ContextError(ctx))
	}
	response, err := decodeResponse(line, method)
	return response, controlDispatchUnknown, err
}

type clientAttachment struct {
	stream    net.Conn
	reader    *bufio.Reader
	control   net.Conn
	closeOnce sync.Once
	closeErr  error
}

func (a *clientAttachment) Read(p []byte) (int, error)  { return a.reader.Read(p) }
func (a *clientAttachment) Write(p []byte) (int, error) { return a.stream.Write(p) }

func (a *clientAttachment) Close() error {
	a.closeOnce.Do(func() { a.closeErr = errors.Join(a.control.Close(), a.stream.Close()) })
	return a.closeErr
}

func (c *Client) Attach(ctx context.Context, ref AttemptRef) (result io.ReadWriteCloser, err error) {
	control, _, err := c.openAttachmentConnection(ctx, attachControlRequest{ref})
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = control.Close() })
	defer func() {
		stop()
		if cause := frame.ContextError(ctx); cause != nil {
			err = errors.Join(err, cause)
		}
		if err != nil {
			_ = control.Close()
			if result != nil {
				_ = result.Close()
				result = nil
			}
		}
	}()
	stream, reader, err := c.openAttachmentConnection(ctx, attachStreamRequest{ref})
	if err != nil {
		return nil, err
	}
	return &clientAttachment{stream: stream, reader: reader, control: control}, nil
}

func (c *Client) openAttachmentConnection(ctx context.Context, request controlRequest) (net.Conn, *bufio.Reader, error) {
	connection, err := c.dial(ctx)
	if err != nil {
		return nil, nil, errors.Join(err, frame.ContextError(ctx))
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	if err := writeRequest(connection, request); err != nil {
		stop()
		_ = connection.Close()
		return nil, nil, errors.Join(err, frame.ContextError(ctx))
	}
	reader := bufio.NewReader(connection)
	line, err := readLine(reader)
	if err == nil {
		_, err = decodeResponse(line, request.method())
	}
	stop()
	if cause := frame.ContextError(ctx); cause != nil {
		err = errors.Join(err, cause)
	}
	if err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	return connection, reader, nil
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	var result []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(result)+len(fragment) > maxControlBytes {
			return nil, frame.ErrCapacity
		}
		result = append(result, fragment...)
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func writeRequest(writer io.Writer, request controlRequest) error {
	data, err := encodeRequest(request)
	if err != nil {
		return err
	}
	return writeControl(writer, data)
}

func writeControl(writer io.Writer, data []byte) error {
	n, err := writer.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

const unclassifiedControlError = "failed"

var controlErrors = []struct {
	code string
	err  error
}{
	{"unknown", ErrUnknown}, {"conflict", ErrConflict}, {"fenced", ErrFenced}, {"attached", ErrAttached},
	{"not_found", ErrNotFound}, {"closed", frame.ErrClosed}, {"capacity", frame.ErrCapacity}, {"invalid", frame.ErrInvalid},
}

func errorCode(err error) string {
	for _, category := range controlErrors {
		if errors.Is(err, category.err) {
			return category.code
		}
	}
	return unclassifiedControlError
}
