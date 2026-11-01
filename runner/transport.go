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

	"github.com/Tangerg/frame"
)

const maxControlBytes = 4 << 20

type controlRequest struct {
	Operation string          `json:"operation"`
	Epoch     uint64          `json:"epoch,omitempty"`
	AttemptID string          `json:"attemptId,omitempty"`
	Launch    *LaunchRequest  `json:"launch,omitempty"`
	Receipt   *ExitReceipt    `json:"receipt,omitempty"`
	Absence   *AbsenceRequest `json:"absence,omitempty"`
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
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	defer workers.Wait()
	defer cancel()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer connection.Close()
			stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
			defer stop()
			s.handle(ctx, connection)
		}()
	}
}

func (s *Server) handle(ctx context.Context, connection net.Conn) {
	reader := bufio.NewReaderSize(connection, 4096)
	line, err := readLine(reader)
	if err != nil {
		_ = writeResponse(connection, nil, err)
		return
	}
	var request controlRequest
	if err := jsonv2.Unmarshal(line, &request, jsonv2.RejectUnknownMembers(true)); err != nil {
		_ = writeResponse(connection, nil, err)
		return
	}
	var observation Observation
	switch request.Operation {
	case "identity":
		identity, identityErr := s.Identity(ctx)
		if identityErr != nil {
			_ = writeResponse(connection, nil, identityErr)
			return
		}
		_ = json.NewEncoder(connection).Encode(controlResponse{Identity: &identity})
		return
	case "fence":
		err = s.Fence(ctx, request.Epoch)
	case "prove_absent":
		if request.Absence == nil {
			_ = writeResponse(connection, nil, frame.ErrInvalid)
			return
		}
		proof, proofErr := s.ProveAbsent(ctx, *request.Absence)
		if proofErr != nil {
			_ = writeResponse(connection, nil, proofErr)
			return
		}
		_ = json.NewEncoder(connection).Encode(controlResponse{Absence: &proof})
		return
	case "acknowledge_exit":
		if request.Receipt == nil {
			err = frame.ErrInvalid
		} else {
			err = s.AcknowledgeExit(ctx, *request.Receipt)
		}
	case "launch":
		if request.Launch == nil {
			err = frame.ErrInvalid
		} else {
			observation, err = s.Launch(ctx, *request.Launch)
		}
	case "inspect":
		observation, err = s.Inspect(ctx, request.AttemptID)
	case "close":
		observation, err = s.CloseAttempt(ctx, request.AttemptID)
	case "attach":
		live, attachErr := s.attach(request.AttemptID, connection)
		if attachErr != nil {
			_ = writeResponse(connection, nil, attachErr)
			return
		}
		if err := writeResponse(connection, nil, nil); err != nil {
			_, _ = s.CloseAttempt(context.Background(), request.AttemptID)
			return
		}
		inputDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(live.stdin, reader)
			_ = live.stdin.Close()
			_, _ = s.CloseAttempt(context.Background(), request.AttemptID)
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
	_ = writeResponse(connection, result, err)
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

func (s *Server) attach(id string, connection io.Closer) (*process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, exists, err := s.lookupAttempt(id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	if item.Observation.State == Unknown {
		return nil, ErrUnknown
	}
	live := s.processes[id]
	if live == nil || live.closing {
		return nil, frame.ErrClosed
	}
	if live.attached {
		return nil, ErrAttached
	}
	live.attached = true
	live.attachment = connection
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
	return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
}

func (c *Client) Identity(ctx context.Context) (Identity, error) {
	response, err := c.request(ctx, controlRequest{Operation: "identity"})
	if err != nil {
		return Identity{}, err
	}
	if response.Identity == nil || !validID(response.Identity.ID) {
		return Identity{}, errors.New("runner: invalid identity response")
	}
	return *response.Identity, nil
}

func (c *Client) Fence(ctx context.Context, epoch uint64) error {
	_, err := c.control(ctx, controlRequest{Operation: "fence", Epoch: epoch})
	return err
}
func (c *Client) Launch(ctx context.Context, request LaunchRequest) (Observation, error) {
	return c.control(ctx, controlRequest{Operation: "launch", Launch: &request})
}
func (c *Client) Inspect(ctx context.Context, id string) (Observation, error) {
	return c.control(ctx, controlRequest{Operation: "inspect", AttemptID: id})
}
func (c *Client) CloseAttempt(ctx context.Context, id string) (Observation, error) {
	return c.control(ctx, controlRequest{Operation: "close", AttemptID: id})
}

func (c *Client) AcknowledgeExit(ctx context.Context, receipt ExitReceipt) error {
	_, err := c.control(ctx, controlRequest{Operation: "acknowledge_exit", Receipt: &receipt})
	return err
}

func (c *Client) ProveAbsent(ctx context.Context, request AbsenceRequest) (AbsenceProof, error) {
	response, err := c.request(ctx, controlRequest{Operation: "prove_absent", Absence: &request})
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

func (c *Client) Attach(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	connection, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	if err := writeRequest(connection, controlRequest{Operation: "attach", AttemptID: id}); err != nil {
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
	if len(data) > maxControlBytes {
		return frame.ErrCapacity
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}

func writeResponse(writer io.Writer, observation *Observation, err error) error {
	response := controlResponse{Observation: observation}
	if err != nil {
		response.Error = errorCode(err)
	}
	return json.NewEncoder(writer).Encode(response)
}

func decodeResponse(data []byte) (controlResponse, error) {
	var response controlResponse
	if err := jsonv2.Unmarshal(data, &response, jsonv2.RejectUnknownMembers(true)); err != nil {
		return response, err
	}
	switch response.Error {
	case "":
		return response, nil
	case "unknown":
		return response, ErrUnknown
	case "conflict":
		return response, ErrConflict
	case "fenced":
		return response, ErrFenced
	case "attached":
		return response, ErrAttached
	case "not_found":
		return response, ErrNotFound
	case "closed":
		return response, frame.ErrClosed
	case "capacity":
		return response, frame.ErrCapacity
	default:
		return response, errors.New("runner: control request failed")
	}
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrUnknown):
		return "unknown"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrFenced):
		return "fenced"
	case errors.Is(err, ErrAttached):
		return "attached"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, frame.ErrClosed):
		return "closed"
	case errors.Is(err, frame.ErrCapacity):
		return "capacity"
	default:
		return "failed"
	}
}
