// Package runner owns actual local child processes by durable attempt identity.
// It has no installation, binding-selection or business-result authority.
package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/localfile"
	framevalue "github.com/Tangerg/frame/value"
)

var (
	ErrUnknown  = errors.New("runner: original process ownership is unknown")
	ErrConflict = errors.New("runner: attempt identity conflict")
	ErrFenced   = errors.New("runner: creation domain is fenced")
	ErrAttached = errors.New("runner: attempt already attached")
	ErrNotFound = errors.New("runner: attempt does not exist")
)

type State string

const (
	Starting State = "starting"
	Running  State = "running"
	Exited   State = "exited"
	Unknown  State = "unknown"
)

type LaunchRequest struct {
	AttemptRef
	Command            string            `json:"command"`
	ExecutableDigest   framevalue.Digest `json:"executableDigest"`
	Args               []string          `json:"args,omitempty"`
	Env                []string          `json:"env"`
	Directory          string            `json:"directory,omitempty"`
	Configuration      framevalue.Digest `json:"configuration"`
	CredentialRevision string            `json:"credentialRevision,omitempty"`
}

type Observation struct {
	AttemptID string `json:"attemptId"`
	Epoch     uint64 `json:"epoch"`
	RunnerID  string `json:"runnerId"`
	State     State  `json:"state"`
	PID       int    `json:"pid,omitempty"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	ProofID   string `json:"proofId,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (o Observation) Reference() AttemptRef {
	return AttemptRef{AttemptID: o.AttemptID, RunnerID: o.RunnerID, Epoch: o.Epoch}
}

func (o Observation) clone() Observation {
	if o.ExitCode != nil {
		o.ExitCode = new(*o.ExitCode)
	}
	return o
}

type Identity struct {
	ID     string `json:"id"`
	Epoch  uint64 `json:"epoch"`
	Fenced bool   `json:"fenced"`
}

func (s *Server) Identity(_ context.Context) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Identity{}, frame.ErrClosed
	}
	return Identity{ID: s.id, Epoch: s.journal.Epoch, Fenced: s.fenced}, nil
}

type Config struct {
	Directory             string
	MaxAttempts           int
	MaxExecutableBytes    int64
	MaxControlConnections int
	ControlTimeout        time.Duration
}

type storedAttempt struct {
	Observation   Observation       `json:"observation"`
	RequestDigest framevalue.Digest `json:"requestDigest"`
}

const (
	journalVersion            = 2
	defaultMaxAttempts        = 4096
	defaultMaxExecutableBytes = 512 << 20
	maxIdentityBytes          = 256
)

type journal struct {
	Version  int                      `json:"version"`
	Epoch    uint64                   `json:"epoch"`
	Attempts map[string]storedAttempt `json:"attempts"`
}

type process struct {
	command  *exec.Cmd
	kill     func() error
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	attached bool

	closeState  processCloseState
	environment []string
	done        chan struct{}
}

type Server struct {
	config        Config
	id            string
	lock          *os.File
	mu            sync.Mutex
	journal       journal
	processes     map[string]*process
	fenced        bool
	closed        bool
	closeOnce     sync.Once
	closeRequests chan chan error
	closeDone     chan struct{}
	closeErr      error
}

func Open(config Config) (_ *Server, err error) {
	if config.Directory == "" {
		return nil, errors.New("runner: state directory is required")
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = defaultMaxAttempts
	}
	if config.MaxExecutableBytes == 0 {
		config.MaxExecutableBytes = defaultMaxExecutableBytes
	}
	if config.MaxControlConnections == 0 {
		config.MaxControlConnections = defaultControlConnections
	}
	if config.ControlTimeout == 0 {
		config.ControlTimeout = defaultControlTimeout
	}
	if config.MaxAttempts < 1 || config.MaxExecutableBytes < 1 || config.MaxControlConnections < 1 || config.ControlTimeout < 0 {
		return nil, frame.ErrCapacity
	}
	config.Directory, err = filepath.Abs(config.Directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.Directory, 0700); err != nil {
		return nil, err
	}
	config.Directory, err = filepath.EvalSymlinks(config.Directory)
	if err != nil {
		return nil, err
	}
	if err := verifyAuthorityDirectory(config.Directory); err != nil {
		return nil, err
	}
	lock, err := lockDirectory(filepath.Join(config.Directory, "authority.lock"))
	if err != nil {
		return nil, err
	}
	server := &Server{config: config, id: randomID(), lock: lock, processes: map[string]*process{}, fenced: true, journal: journal{Version: journalVersion, Attempts: map[string]storedAttempt{}}}
	defer func() {
		if err != nil {
			err = errors.Join(err, unlockDirectory(lock))
		}
	}()
	data, readErr := os.ReadFile(filepath.Join(config.Directory, "attempts.json"))
	if readErr == nil {
		if err := jsonv2.Unmarshal(data, &server.journal, jsonv2.RejectUnknownMembers(true)); err != nil {
			return nil, fmt.Errorf("runner: invalid journal: %w", err)
		}
		if server.journal.Version != journalVersion || server.journal.Attempts == nil || len(server.journal.Attempts) > config.MaxAttempts {
			return nil, errors.New("runner: incompatible journal")
		}
		for id, attempt := range server.journal.Attempts {
			if id != attempt.Observation.AttemptID || !validID(id) || attempt.RequestDigest.Validate() != nil {
				return nil, errors.New("runner: invalid attempt record")
			}
			switch attempt.Observation.State {
			case Starting, Running:
				attempt.Observation.State = Unknown
				attempt.Observation.Reason = "runner restarted without the original child wait identity"
				attempt.Observation.PID = 0
				server.journal.Attempts[id] = attempt
			case Exited, Unknown:
			default:
				return nil, errors.New("runner: invalid attempt state")
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if err := server.persist(); err != nil {
		return nil, err
	}
	return server, nil
}

// Fence settles all earlier Launch entries before advancing the creation epoch.
// Existing processes keep their original close and wait path. It never kills a
// PID recovered from disk, nor permits a missing old attempt to be recreated.
func (s *Server) Fence(_ context.Context, epoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || epoch < s.journal.Epoch || epoch == 0 {
		return ErrFenced
	}
	if epoch == s.journal.Epoch {
		if s.fenced {
			return ErrFenced
		}
		return nil
	}
	s.fenced = true
	s.journal.Epoch = epoch
	if err := s.persist(); err != nil {
		return err
	}
	s.fenced = false
	return nil
}

func (s *Server) Launch(ctx context.Context, request LaunchRequest) (_ Observation, err error) {
	if err := validateLaunch(request); err != nil {
		return Observation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	digest, err := requestIdentity(request)
	if err != nil {
		return Observation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, found, err := s.replayLaunch(request, digest); err != nil || found {
		return previous, err
	}
	if s.closed || s.fenced || request.RunnerID != s.id || request.Epoch != s.journal.Epoch {
		return Observation{}, ErrFenced
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, context.Cause(ctx)
	}
	if len(s.journal.Attempts) >= s.config.MaxAttempts {
		return Observation{}, frame.ErrCapacity
	}
	observation := Observation{AttemptID: request.AttemptID, Epoch: request.Epoch, RunnerID: s.id, State: Starting}
	s.journal.Attempts[request.AttemptID] = storedAttempt{Observation: observation, RequestDigest: digest}
	if err := s.persist(); err != nil {
		s.fenced = true
		return observation, errors.Join(ErrUnknown, err)
	}
	path, err := s.snapshotExecutable(request)
	if err != nil {
		return s.failedStart(request.AttemptID, "executable verification failed", err)
	}
	started := false
	defer func() {
		if !started {
			err = errors.Join(err, releaseExecutable(path))
		}
	}()
	command := exec.Command(path, request.Args...)
	command.Env = append([]string{}, request.Env...)
	command.Dir = request.Directory
	stdin, err := command.StdinPipe()
	if err != nil {
		return s.failedStart(request.AttemptID, "stdin creation failed", err)
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return s.failedStart(request.AttemptID, "stdout creation failed", err)
	}
	command.Stdout = stdoutWriter
	// Diagnostics must not fill a pipe and stall a process before Attach. The
	// host may explicitly provide structured logging through its plugin contract.
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return s.failedStart(request.AttemptID, "process creation failed", err)
	}
	started = true
	_ = stdoutWriter.Close()
	live := &process{command: command, kill: command.Process.Kill, stdin: stdin, stdout: stdout, environment: slices.Clone(request.Env), done: make(chan struct{})}
	s.processes[request.AttemptID] = live
	observation.State = Running
	observation.PID = command.Process.Pid
	record := s.journal.Attempts[request.AttemptID]
	record.Observation = observation
	s.journal.Attempts[request.AttemptID] = record
	persistErr := s.persist()
	if persistErr != nil {
		s.fenced = true
		persistErr = errors.Join(persistErr, live.requestClose())
	}
	go s.wait(request.AttemptID, live, path)
	if persistErr != nil {
		return observation, errors.Join(ErrUnknown, persistErr)
	}
	return observation, nil
}

func (s *Server) replayLaunch(request LaunchRequest, digest framevalue.Digest) (Observation, bool, error) {
	old, found, err := s.lookupAttempt(request.AttemptID)
	if err != nil || !found {
		return Observation{}, false, err
	}
	if old.RequestDigest != digest {
		return Observation{}, true, ErrConflict
	}
	if live := s.processes[request.AttemptID]; live != nil && !slices.Equal(live.environment, request.Env) {
		return Observation{}, true, ErrConflict
	}
	return old.Observation.clone(), true, nil
}

func (s *Server) Inspect(_ context.Context, id string) (Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, exists, err := s.lookupAttempt(id)
	if err != nil {
		return Observation{}, err
	}
	if !exists {
		return Observation{}, ErrNotFound
	}
	return item.Observation.clone(), nil
}

func (s *Server) CloseAttempt(ctx context.Context, ref AttemptRef) (Observation, error) {
	s.mu.Lock()
	item, exists, err := s.lookupAttempt(ref.AttemptID)
	if err != nil {
		s.mu.Unlock()
		return Observation{}, err
	}
	if !exists {
		s.mu.Unlock()
		return Observation{}, ErrNotFound
	}
	if item.Observation.Reference() != ref {
		s.mu.Unlock()
		return Observation{}, ErrFenced
	}
	if item.Observation.State == Unknown {
		s.mu.Unlock()
		return item.Observation.clone(), ErrUnknown
	}
	if item.Observation.State == Exited {
		s.mu.Unlock()
		return item.Observation.clone(), nil
	}
	live := s.processes[ref.AttemptID]
	if live == nil {
		s.mu.Unlock()
		return item.Observation.clone(), ErrUnknown
	}
	if err := live.requestClose(); err != nil {
		s.mu.Unlock()
		return item.Observation.clone(), err
	}
	done := live.done
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return item.Observation.clone(), ctx.Err()
	case <-done:
		return s.Inspect(context.Background(), ref.AttemptID)
	}
}

func (s *Server) wait(id string, live *process, path string) {
	err := live.command.Wait()
	_ = live.stdin.Close()
	exited := live.command.ProcessState != nil
	var cleanupErr error
	if exited {
		cleanupErr = releaseExecutable(path)
	}
	s.mu.Lock()
	item := s.journal.Attempts[id]
	if exited {
		item.Observation.State = Exited
		item.Observation.ExitCode = new(live.command.ProcessState.ExitCode())
		item.Observation.ProofID = randomID()
		item.Observation.Reason = "direct child wait completed"
		if cleanupErr != nil {
			item.Observation.Reason += "; " + cleanupErr.Error()
		}
		delete(s.processes, id)
	} else {
		live.closeState = processWaitUnconfirmed
		s.fenced = true
		item.Observation.State = Unknown
		item.Observation.ExitCode = nil
		item.Observation.ProofID = ""
		item.Observation.Reason = "child wait failed without an exit observation"
		if err != nil {
			item.Observation.Reason += ": " + err.Error()
		}
	}
	_, _ = s.publishTerminal(id, item)
	attached := live.attached
	close(live.done)
	s.mu.Unlock()
	if exited && !attached {
		_ = live.stdout.Close()
	}
}

func releaseExecutable(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("runner: executable snapshot cleanup failed: %w", err)
	}
	return nil
}

func (s *Server) failedStart(id, reason string, cause error) (Observation, error) {
	item := s.journal.Attempts[id]
	item.Observation.State = Exited
	item.Observation.ProofID = randomID()
	item.Observation.Reason = reason
	observed, err := s.publishTerminal(id, item)
	return observed, errors.Join(cause, err)
}

func (s *Server) publishTerminal(id string, item storedAttempt) (Observation, error) {
	s.journal.Attempts[id] = item
	if err := s.persist(); err != nil {
		s.fenced = true
		item.Observation.State = Unknown
		item.Observation.ProofID = ""
		item.Observation.ExitCode = nil
		item.Observation.Reason = "exit evidence could not be persisted"
		s.journal.Attempts[id] = item
		return item.Observation.clone(), errors.Join(ErrUnknown, err)
	}
	return item.Observation.clone(), nil
}

func (s *Server) snapshotExecutable(request LaunchRequest) (_ string, err error) {
	input, err := localfile.OpenRegular(request.Command)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > s.config.MaxExecutableBytes {
		return "", errors.New("runner: invalid executable")
	}
	file, err := os.CreateTemp(s.config.Directory, "executable-")
	if err != nil {
		return "", err
	}
	name := file.Name()
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			err = errors.Join(err, releaseExecutable(name))
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(input, s.config.MaxExecutableBytes+1))
	if err != nil {
		return "", err
	}
	if n > s.config.MaxExecutableBytes || framevalue.DigestOf([sha256.Size]byte(hash.Sum(nil))) != request.ExecutableDigest {
		return "", errors.New("runner: executable digest mismatch")
	}
	if err := file.Chmod(0500); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Server) persist() error {
	return storeRecord(filepath.Join(s.config.Directory, "attempts.json"), s.journal)
}

func storeRecord(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".record-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func validateLaunch(request LaunchRequest) error {
	if !request.AttemptRef.valid() || !filepath.IsAbs(request.Command) || request.ExecutableDigest.Validate() != nil || request.Configuration.Validate() != nil {
		return errors.New("runner: exact launch identity is required")
	}
	if request.Directory != "" && !filepath.IsAbs(request.Directory) {
		return errors.New("runner: working directory must be absolute")
	}
	if request.CredentialRevision != "" && !validID(request.CredentialRevision) {
		return errors.New("runner: invalid credential revision")
	}
	names := map[string]bool{}
	for _, entry := range request.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, '\x00') || names[key] {
			return errors.New("runner: invalid or duplicate environment key")
		}
		names[key] = true
	}
	return nil
}

func requestIdentity(request LaunchRequest) (framevalue.Digest, error) {
	copy := request
	// Credential values never enter the durable identity. While the child is live,
	// its original injected environment is checked directly on replay.
	copy.Env = nil
	data, err := json.Marshal(copy)
	if err != nil {
		return framevalue.Digest{}, err
	}
	return framevalue.Hash(data), nil
}

func validID(value string) bool {
	return value != "" && len(value) <= maxIdentityBytes && !strings.ContainsAny(value, "\x00\r\n\t ")
}
func randomID() string {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}
