// Package runner owns actual local child processes by durable attempt identity.
// It has no installation, binding-selection or business-result authority.
package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/localfile"
	framevalue "github.com/Tangerg/frame/value"
)

var (
	ErrUnknown     = errors.New("runner: original process ownership is unknown")
	ErrConflict    = errors.New("runner: attempt identity conflict")
	ErrFenced      = errors.New("runner: creation domain is fenced")
	ErrAttached    = errors.New("runner: attempt already attached")
	ErrNotFound    = errors.New("runner: attempt does not exist")
	ErrUnavailable = errors.New("runner: original process is unavailable")
)

type State string

const (
	Starting   State = "starting"
	Running    State = "running"
	NotCreated State = "not_created"
	Exited     State = "exited"
	Unknown    State = "unknown"
)

type Observation struct {
	AttemptRef
	State    State  `json:"state"`
	PID      int    `json:"pid,omitzero"`
	ExitCode *int   `json:"exitCode,omitempty"`
	ProofID  string `json:"proofId,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// presence states whether an observation must carry, must omit or may carry
// one member of its evidence.
type presence uint8

const (
	absent presence = iota
	present
	either
)

func (p presence) admits(carried bool) bool { return p == either || (p == present) == carried }

type observationEvidence struct{ pid, exitCode, proof presence }

// evidence is the single statement of what each state carries. A runner that
// lost its wait identity may still report the PID it last observed.
func (s State) evidence() (observationEvidence, bool) {
	switch s {
	case Starting:
		return observationEvidence{}, true
	case Running:
		return observationEvidence{pid: present}, true
	case NotCreated:
		return observationEvidence{proof: present}, true
	case Exited:
		return observationEvidence{pid: present, exitCode: present, proof: present}, true
	case Unknown:
		return observationEvidence{pid: either}, true
	default:
		return observationEvidence{}, false
	}
}

func (o Observation) validate() error {
	evidence, known := o.State.evidence()
	if !known || o.AttemptRef.Validate() != nil || o.PID < 0 {
		return frame.ErrInvalid
	}
	if !evidence.pid.admits(o.PID != 0) || !evidence.exitCode.admits(o.ExitCode != nil) || !evidence.proof.admits(o.ProofID != "") {
		return frame.ErrInvalid
	}
	if evidence.proof == present && !frame.ValidName(o.ProofID) {
		return frame.ErrInvalid
	}
	return nil
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

type process struct {
	command    *exec.Cmd
	kill       func() error
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	attachment *attachment

	closeState  processCloseState
	environment []string
	done        chan struct{}
}

const (
	defaultMaxAttempts        = 4096
	defaultMaxExecutableBytes = 512 << 20
)

type Server struct {
	config               Config
	id                   string
	directory            *os.Root
	lock                 *os.File
	mu                   sync.Mutex
	journal              journal
	processes            map[string]*process
	terminalObservations map[string]Observation
	listener             *controlListener
	fenced               bool
	closed               bool
	closeOnce            sync.Once
	closeRequests        chan chan error
	closeDone            chan struct{}
	closeErr             error
}

func Open(config Config) (_ *Server, err error) {
	config, err = config.resolve()
	if err != nil {
		return nil, err
	}
	directory, err := os.OpenRoot(config.Directory)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, directory.Close())
		}
	}()
	lock, err := directory.Open(".")
	if err != nil {
		return nil, err
	}
	if err := lockDirectory(lock); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, unlockDirectory(lock))
		}
	}()
	journal, err := loadJournal(directory, config.MaxAttempts)
	if err != nil {
		return nil, err
	}
	server := &Server{
		config: config, id: rand.Text(), directory: directory, lock: lock, fenced: true, journal: journal,
		processes: make(map[string]*process), terminalObservations: make(map[string]Observation),
	}
	if err := server.restoreReceipts(); err != nil {
		return nil, err
	}
	if err := server.persist(); err != nil {
		return nil, err
	}
	return server, nil
}

func (c Config) resolve() (Config, error) {
	if c.Directory == "" {
		return c, errors.New("runner: state directory is required")
	}
	var err error
	if c.MaxAttempts, err = frame.ResolveLimit("runner attempts", c.MaxAttempts, defaultMaxAttempts); err != nil {
		return c, err
	}
	if c.MaxControlConnections, err = frame.ResolveLimit("runner control connections", c.MaxControlConnections, defaultControlConnections); err != nil {
		return c, err
	}
	if c.MaxExecutableBytes, err = frame.ResolveLimit("runner executable bytes", c.MaxExecutableBytes, defaultMaxExecutableBytes); err != nil {
		return c, err
	}
	// Snapshotting reads one byte past the limit to detect an oversized executable.
	if c.MaxExecutableBytes == math.MaxInt64 {
		return c, fmt.Errorf("%w: runner executable bytes limit", frame.ErrInvalid)
	}
	if c.ControlTimeout, err = frame.ResolveLimit("runner control timeout", c.ControlTimeout, defaultControlTimeout); err != nil {
		return c, err
	}
	directory, err := filepath.Abs(c.Directory)
	if err != nil {
		return c, err
	}
	if err := os.MkdirAll(directory, localfile.PrivateDirectoryMode); err != nil {
		return c, err
	}
	if c.Directory, err = filepath.EvalSymlinks(directory); err != nil {
		return c, err
	}
	return c, verifyAuthorityDirectory(c.Directory)
}

// Fence settles all earlier Launch entries before advancing the creation epoch.
// Existing processes keep their original close and wait path. It never kills a
// PID recovered from disk, nor permits a missing old attempt to be recreated.
func (s *Server) Fence(_ context.Context, epoch uint64) error {
	if err := (fenceRequest{Epoch: epoch}).Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || epoch < s.journal.Epoch {
		return ErrFenced
	}
	if err := s.verifyDirectory(); err != nil {
		return err
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

func (s *Server) Launch(ctx context.Context, request LaunchRequest) (_ LaunchResult, err error) {
	if err := request.Validate(); err != nil {
		return request.notDispatched(), err
	}
	if err := frame.ContextError(ctx); err != nil {
		return request.notDispatched(), err
	}
	digest, err := request.durableIdentity()
	if err != nil {
		return request.notDispatched(), err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, found, err := s.replayLaunch(request, digest); err != nil || found {
		return LaunchResult{Observation: previous}, err
	}
	if err := s.admitLaunchLocked(ctx, request); err != nil {
		return request.notDispatched(), err
	}
	observation := Observation{AttemptRef: request.AttemptRef, State: Starting}
	full := len(s.journal.Attempts) >= s.config.MaxAttempts
	s.journal.Attempts[request.AttemptID] = storedAttempt{Observation: observation, RequestDigest: digest}
	if full {
		return s.failedStart(request.AttemptID, "outstanding attempt capacity reached", frame.ErrCapacity)
	}
	if err := s.persist(); err != nil {
		s.fenced = true
		return s.failedStart(request.AttemptID, "attempt reservation failed", err)
	}
	path, err := s.snapshotExecutable(request)
	if err != nil {
		return s.failedStart(request.AttemptID, "executable verification failed", err)
	}
	started := false
	defer func() {
		if !started {
			err = errors.Join(err, s.releaseExecutable(path))
		}
	}()
	if err := frame.ContextError(ctx); err != nil {
		return s.failedStart(request.AttemptID, "launch canceled before process creation", err)
	}
	if err := s.verifyDirectory(); err != nil {
		return s.failedStart(request.AttemptID, "authority directory replaced before process creation", err)
	}
	live, reason, err := spawn(filepath.Join(s.config.Directory, path), request)
	if err != nil {
		return s.failedStart(request.AttemptID, reason, err)
	}
	started = true
	s.processes[request.AttemptID] = live
	observation.State = Running
	observation.PID = live.command.Process.Pid
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
		return LaunchResult{Observation: observation}, errors.Join(ErrUnknown, persistErr)
	}
	return LaunchResult{Observation: observation}, nil
}

// admitLaunchLocked refuses a request this runner may not dispatch: another
// runner's or epoch's attempt, a fenced authority or a replaced directory.
func (s *Server) admitLaunchLocked(ctx context.Context, request LaunchRequest) error {
	if s.closed || s.fenced || request.RunnerID != s.id || request.Epoch != s.journal.Epoch {
		return ErrFenced
	}
	if err := s.verifyDirectory(); err != nil {
		return err
	}
	return frame.ContextError(ctx)
}

// spawn starts the verified executable with only stdin and stdout connected.
// Diagnostics must not fill a pipe and stall a process before Attach, and a
// stderr copy pipe would make Wait depend on descriptors inherited by
// descendants. On failure it returns the persisted reason.
func spawn(path string, request LaunchRequest) (*process, string, error) {
	command := exec.Command(path, request.Args...)
	// A nil Env would inherit the runner's environment.
	command.Env = append([]string{}, request.Env...)
	command.Dir = request.Directory
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, "stdin creation failed", err
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, "stdout creation failed", err
	}
	command.Stdout = stdoutWriter
	err = command.Start()
	_ = stdoutWriter.Close()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, "process creation failed", err
	}
	live := &process{command: command, kill: command.Process.Kill, stdin: stdin, stdout: stdout, environment: slices.Clone(request.Env), done: make(chan struct{})}
	return live, "", nil
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
	observation, err := s.inspect(request.AttemptID)
	return observation, true, err
}

func (s *Server) Inspect(_ context.Context, id string) (Observation, error) {
	if err := (inspectRequest{AttemptID: id}).Validate(); err != nil {
		return Observation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inspect(id)
}

func (s *Server) inspect(id string) (Observation, error) {
	if _, pending := s.terminalObservations[id]; pending {
		return s.commitTerminal(id)
	}
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
	if err := ref.Validate(); err != nil {
		return Observation{}, err
	}
	if framevalue.IsNil(ctx) {
		return Observation{}, frame.ErrInvalid
	}
	s.mu.Lock()
	observation, err := s.inspect(ref.AttemptID)
	if err != nil {
		s.mu.Unlock()
		return observation, err
	}
	if observation.AttemptRef != ref {
		s.mu.Unlock()
		return Observation{}, ErrFenced
	}
	if observation.State == Unknown {
		s.mu.Unlock()
		return observation, ErrUnknown
	}
	if observation.State == Exited || observation.State == NotCreated {
		s.mu.Unlock()
		return observation, nil
	}
	live := s.processes[ref.AttemptID]
	if live == nil {
		s.mu.Unlock()
		return observation, ErrUnknown
	}
	if err := live.requestClose(); err != nil {
		s.mu.Unlock()
		return observation, err
	}
	done := live.done
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		select {
		case <-done:
			return s.Inspect(context.Background(), ref.AttemptID)
		default:
			return observation, frame.ContextError(ctx)
		}
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
		cleanupErr = s.releaseExecutable(path)
	}
	s.mu.Lock()
	item := s.journal.Attempts[id]
	if exited {
		item.Observation.State = Exited
		item.Observation.ExitCode = new(live.command.ProcessState.ExitCode())
		item.Observation.ProofID = rand.Text()
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
	_, _ = s.publishTerminal(id, item.Observation)
	if exited && live.stdout != nil {
		_ = live.stdout.Close()
		live.stdout = nil
	}
	if live.attachment != nil {
		live.attachment.closeUnconnected()
	}
	close(live.done)
	s.mu.Unlock()
}

func (s *Server) failedStart(id, reason string, cause error) (LaunchResult, error) {
	item := s.journal.Attempts[id]
	item.Observation.State = NotCreated
	item.Observation.ProofID = rand.Text()
	item.Observation.Reason = reason
	observed, err := s.publishTerminal(id, item.Observation)
	return LaunchResult{Observation: observed}, errors.Join(cause, err)
}

func (s *Server) publishTerminal(id string, observation Observation) (Observation, error) {
	state := s.journal.Attempts[id].Observation.State
	if _, pending := s.terminalObservations[id]; pending || state == Exited || state == NotCreated {
		return Observation{}, ErrConflict
	}
	// Persistence may delay publication, but cannot replace the original Wait's
	// observation or mint another proof for it.
	s.terminalObservations[id] = observation
	return s.commitTerminal(id)
}

func (s *Server) commitTerminal(id string) (Observation, error) {
	observation := s.terminalObservations[id]
	if err := s.persist(); err != nil {
		s.fenced = true
		return Observation{
			AttemptID: observation.AttemptID, Epoch: observation.Epoch, RunnerID: observation.RunnerID,
			State: Unknown, Reason: "terminal observation is not durable",
		}, errors.Join(ErrUnknown, err)
	}
	return observation.clone(), nil
}

// Directory handles keep persistence and cleanup on the original inode even
// after a pathname is replaced. Path-based process and socket creation must
// still refuse a directory that no longer names this authority.
func (s *Server) verifyDirectory() error {
	original, err := s.lock.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(s.config.Directory)
	if err != nil || !os.SameFile(original, current) {
		s.fenced = true
		return errors.Join(ErrFenced, err)
	}
	return verifyAuthorityDirectory(s.config.Directory)
}
