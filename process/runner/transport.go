package runner

import (
	"bufio"
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
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

type controlRequest struct {
	Operation controlMethod  `json:"operation"`
	Epoch     uint64         `json:"epoch,omitzero"`
	AttemptID string         `json:"attemptId,omitempty"`
	Launch    *LaunchRequest `json:"launch,omitempty"`
	Receipt   *ExitReceipt   `json:"receipt,omitempty"`
	Attempt   *AttemptRef    `json:"attempt,omitempty"`
}

type controlResponse struct {
	Observation *Observation  `json:"observation,omitempty"`
	Identity    *Identity     `json:"identity,omitempty"`
	Absence     *AbsenceProof `json:"absence,omitempty"`
	Error       string        `json:"error,omitempty"`
}

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
			return cancellation.Err(ctx)
		case slots <- struct{}{}:
		}
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
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
		_ = s.writeResponse(connection, nil, err)
		return
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	if request.Operation == methodAttachControl || request.Operation == methodAttachStream {
		if request.Attempt == nil {
			_ = s.writeResponse(connection, nil, frame.ErrInvalid)
			return
		}
		if request.Operation == methodAttachControl {
			s.serveAttachmentControl(ctx, connection, reader, *request.Attempt, release)
		} else {
			s.serveAttachmentStream(ctx, connection, reader, *request.Attempt, release)
		}
		return
	}
	response, err := s.respond(ctx, request)
	if err != nil {
		response.Error = errorCode(err)
	}
	_ = s.writeReply(connection, response)
}

func readRequest(reader *bufio.Reader) (controlRequest, error) {
	line, err := readLine(reader)
	if err != nil {
		return controlRequest{}, err
	}
	var request controlRequest
	return request, jsonv2.Unmarshal(line, &request, jsonv2.RejectUnknownMembers(true))
}

func (s *Server) respond(ctx context.Context, request controlRequest) (controlResponse, error) {
	switch request.Operation {
	case methodIdentity:
		identity, err := s.Identity(ctx)
		if err != nil {
			return controlResponse{}, err
		}
		return controlResponse{Identity: &identity}, nil
	case methodFence:
		return controlResponse{}, s.Fence(ctx, request.Epoch)
	case methodProveAbsent:
		if request.Attempt == nil {
			return controlResponse{}, frame.ErrInvalid
		}
		proof, err := s.ProveAbsent(ctx, *request.Attempt)
		if err != nil {
			return controlResponse{}, err
		}
		return controlResponse{Absence: &proof}, nil
	case methodAcknowledgeExit:
		if request.Receipt == nil {
			return controlResponse{}, frame.ErrInvalid
		}
		return controlResponse{}, s.AcknowledgeExit(ctx, *request.Receipt)
	case methodLaunch:
		if request.Launch == nil {
			return controlResponse{}, frame.ErrInvalid
		}
		return observed(s.Launch(ctx, *request.Launch))
	case methodInspect:
		return observed(s.Inspect(ctx, request.AttemptID))
	case methodClose:
		if request.Attempt == nil {
			return controlResponse{}, frame.ErrInvalid
		}
		return observed(s.CloseAttempt(ctx, *request.Attempt))
	default:
		return controlResponse{}, frame.ErrInvalid
	}
}

// observed reports an observation even beside an error, so a failed close
// still tells the caller the attempt's current state.
func observed(observation Observation, err error) (controlResponse, error) {
	if observation.AttemptID == "" {
		return controlResponse{}, err
	}
	return controlResponse{Observation: &observation}, err
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

func Dial(socket string) *Client { return &Client{socket: socket} }

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
	if err := validateSocketPath(c.socket); err != nil {
		return nil, err
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
	response, err := c.request(ctx, controlRequest{Operation: methodIdentity})
	if err != nil {
		return Identity{}, err
	}
	if response.Identity == nil || !frame.ValidName(response.Identity.ID) {
		return Identity{}, errors.New("runner: invalid identity response")
	}
	return *response.Identity, nil
}

func (c *Client) Fence(ctx context.Context, epoch uint64) error {
	_, err := c.control(ctx, controlRequest{Operation: methodFence, Epoch: epoch})
	return err
}
func (c *Client) Launch(ctx context.Context, request LaunchRequest) (Observation, error) {
	return c.control(ctx, controlRequest{Operation: methodLaunch, Launch: &request})
}
func (c *Client) Inspect(ctx context.Context, id string) (Observation, error) {
	return c.control(ctx, controlRequest{Operation: methodInspect, AttemptID: id})
}
func (c *Client) CloseAttempt(ctx context.Context, ref AttemptRef) (Observation, error) {
	return c.control(ctx, controlRequest{Operation: methodClose, Attempt: &ref})
}

func (c *Client) AcknowledgeExit(ctx context.Context, receipt ExitReceipt) error {
	_, err := c.control(ctx, controlRequest{Operation: methodAcknowledgeExit, Receipt: &receipt})
	return err
}

func (c *Client) ProveAbsent(ctx context.Context, request AttemptRef) (AbsenceProof, error) {
	response, err := c.request(ctx, controlRequest{Operation: methodProveAbsent, Attempt: &request})
	if err != nil {
		return AbsenceProof{}, err
	}
	if response.Absence == nil || response.Absence.Validate(request) != nil {
		return AbsenceProof{}, ErrUnknown
	}
	return *response.Absence, nil
}

func (c *Client) control(ctx context.Context, request controlRequest) (Observation, error) {
	response, err := c.request(ctx, request)
	var observation Observation
	if response.Observation != nil {
		observation = *response.Observation
	}
	return observation, err
}

func (c *Client) request(ctx context.Context, request controlRequest) (controlResponse, error) {
	connection, err := c.dial(ctx)
	if err != nil {
		return controlResponse{}, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := writeRequest(connection, request); err != nil {
		return controlResponse{}, err
	}
	line, err := readLine(bufio.NewReader(connection))
	if err != nil {
		return controlResponse{}, err
	}
	return decodeResponse(line)
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
	control, _, err := c.openAttachmentConnection(ctx, methodAttachControl, ref)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = control.Close() })
	defer func() {
		stop()
		if cause := cancellation.Err(ctx); cause != nil {
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
	stream, reader, err := c.openAttachmentConnection(ctx, methodAttachStream, ref)
	if err != nil {
		return nil, err
	}
	return &clientAttachment{stream: stream, reader: reader, control: control}, nil
}

func (c *Client) openAttachmentConnection(ctx context.Context, method controlMethod, ref AttemptRef) (net.Conn, *bufio.Reader, error) {
	connection, err := c.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	if err := writeRequest(connection, controlRequest{Operation: method, Attempt: &ref}); err != nil {
		stop()
		_ = connection.Close()
		return nil, nil, errors.Join(err, cancellation.Err(ctx))
	}
	reader := bufio.NewReader(connection)
	line, err := readLine(reader)
	if err == nil {
		_, err = decodeResponse(line)
	}
	stop()
	if cause := cancellation.Err(ctx); cause != nil {
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
	data, err := jsonv2.Marshal(request)
	if err != nil {
		return err
	}
	if len(data)+1 > maxControlBytes {
		return frame.ErrCapacity
	}
	n, err := writer.Write(append(data, '\n'))
	if err == nil && n != len(data)+1 {
		return io.ErrShortWrite
	}
	return err
}

func (s *Server) writeResponse(writer net.Conn, observation *Observation, err error) error {
	response := controlResponse{Observation: observation}
	if err != nil {
		response.Error = errorCode(err)
	}
	return s.writeReply(writer, response)
}

func (s *Server) writeReply(writer net.Conn, response controlResponse) error {
	if err := writer.SetWriteDeadline(time.Now().Add(s.config.ControlTimeout)); err != nil {
		return err
	}
	return json.NewEncoder(writer).Encode(response)
}

func decodeResponse(data []byte) (controlResponse, error) {
	var response controlResponse
	if err := jsonv2.Unmarshal(data, &response, jsonv2.RejectUnknownMembers(true)); err != nil {
		return response, err
	}
	if response.Error == "" {
		return response, nil
	}
	for _, category := range controlErrors {
		if category.code == response.Error {
			return response, category.err
		}
	}
	return response, errors.New("runner: control request failed")
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
