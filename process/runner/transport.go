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
	"os"
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
	methodAttach          controlMethod = "attach"
)

type controlRequest struct {
	Operation controlMethod  `json:"operation"`
	Epoch     uint64         `json:"epoch,omitempty"`
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

// Listen uses a private Unix socket. A runner has one local authority; remote
// clients and authenticated multi-user execution are not this profile.
func Listen(socket string) (net.Listener, error) {
	if err := validateSocketPath(socket); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return nil, err
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil {
		return nil, err
	}
	if err := verifyAuthorityDirectory(directory); err != nil {
		return nil, err
	}
	socket = filepath.Join(directory, filepath.Base(socket))
	if _, err := os.Lstat(socket); err == nil {
		return nil, errors.New("runner: socket already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	socket, ok := listener.(*net.UnixListener)
	if ctx == nil || !ok || socket == nil {
		return frame.ErrInvalid
	}
	path := socket.Addr().String()
	if err := validateSocketPath(path); err != nil {
		return err
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	if directory != s.config.Directory {
		return errors.New("runner: listener is outside the owned state directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("runner: listener must be a private Unix socket")
	}

	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	slots := make(chan struct{}, s.config.MaxControlConnections)
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	defer workers.Wait()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
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
	line, err := readLine(reader)
	if err != nil {
		_ = s.writeResponse(connection, nil, err)
		return
	}
	var request controlRequest
	if err := jsonv2.Unmarshal(line, &request, jsonv2.RejectUnknownMembers(true)); err != nil {
		_ = s.writeResponse(connection, nil, err)
		return
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	var observation Observation
	switch request.Operation {
	case methodIdentity:
		identity, identityErr := s.Identity(ctx)
		if identityErr != nil {
			_ = s.writeResponse(connection, nil, identityErr)
			return
		}
		_ = s.writeReply(connection, controlResponse{Identity: &identity})
		return
	case methodFence:
		err = s.Fence(ctx, request.Epoch)
	case methodProveAbsent:
		if request.Attempt == nil {
			_ = s.writeResponse(connection, nil, frame.ErrInvalid)
			return
		}
		proof, proofErr := s.ProveAbsent(ctx, *request.Attempt)
		if proofErr != nil {
			_ = s.writeResponse(connection, nil, proofErr)
			return
		}
		_ = s.writeReply(connection, controlResponse{Absence: &proof})
		return
	case methodAcknowledgeExit:
		if request.Receipt == nil {
			err = frame.ErrInvalid
		} else {
			err = s.AcknowledgeExit(ctx, *request.Receipt)
		}
	case methodLaunch:
		if request.Launch == nil {
			err = frame.ErrInvalid
		} else {
			observation, err = s.Launch(ctx, *request.Launch)
		}
	case methodInspect:
		observation, err = s.Inspect(ctx, request.AttemptID)
	case methodClose:
		if request.Attempt == nil {
			err = frame.ErrInvalid
			break
		}
		observation, err = s.CloseAttempt(ctx, *request.Attempt)
	case methodAttach:
		if request.Attempt == nil {
			err = frame.ErrInvalid
			break
		}
		live, attachErr := s.attach(*request.Attempt)
		if attachErr != nil {
			_ = s.writeResponse(connection, nil, attachErr)
			return
		}
		if err := s.writeResponse(connection, nil, nil); err != nil {
			_, _ = s.CloseAttempt(context.Background(), *request.Attempt)
			return
		}
		if err := connection.SetDeadline(time.Time{}); err != nil {
			_, _ = s.CloseAttempt(ctx, *request.Attempt)
			return
		}
		release()
		inputDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(live.stdin, reader)
			_ = live.stdin.Close()
			_, _ = s.CloseAttempt(context.Background(), *request.Attempt)
			close(inputDone)
		}()
		_, _ = io.Copy(connection, live.stdout)
		_ = live.stdout.Close()
		_ = connection.Close()
		<-inputDone
		return
	default:
		err = frame.ErrInvalid
	}
	var result *Observation
	if observation.AttemptID != "" {
		result = &observation
	}
	_ = s.writeResponse(connection, result, err)
}

func (s *Server) Listen(socket string) (net.Listener, error) {
	if err := validateSocketPath(socket); err != nil {
		return nil, err
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil {
		return nil, err
	}
	if directory != s.config.Directory {
		return nil, errors.New("runner: socket must be in the owned state directory")
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("runner: control path is not a socket")
		}
		if err := os.Remove(socket); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return Listen(socket)
}

func (s *Server) attach(ref AttemptRef) (*process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if live.attached {
		return nil, ErrAttached
	}
	live.attached = true

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
	if response.Identity == nil || !validID(response.Identity.ID) {
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

type attached struct {
	net.Conn
	reader *bufio.Reader
}

func (a *attached) Read(p []byte) (int, error) { return a.reader.Read(p) }

func (c *Client) Attach(ctx context.Context, ref AttemptRef) (io.ReadWriteCloser, error) {
	connection, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	if err := writeRequest(connection, controlRequest{Operation: methodAttach, Attempt: &ref}); err != nil {
		stop()
		_ = connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	line, err := readLine(reader)
	if err == nil {
		_, err = decodeResponse(line)
	}
	if !stop() && ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	return &attached{Conn: connection, reader: reader}, nil
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
	data, err := json.Marshal(request)
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
	return "failed"
}
