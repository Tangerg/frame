// Package runner owns actual local child processes by durable attempt identity.
// It has no installation, binding-selection or business-result authority.
package runner

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
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
	Starting   State = "starting"
	Running    State = "running"
	NotCreated State = "not_created"
	Exited     State = "exited"
	Unknown    State = "unknown"
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

// LaunchResult distinguishes this invocation's undispatched request from an
// original runner's durable observation. An undispatched retry says nothing
// about whether an earlier invocation of the same request was accepted.
type LaunchResult struct {
	Observation
	undispatchedRequest *framevalue.Digest
}

func (result LaunchResult) NotDispatched(request LaunchRequest) bool {
	if result.Observation != (Observation{}) || result.undispatchedRequest == nil {
		return false
	}
	data, err := jsonv2.Marshal(request)
	return err == nil && framevalue.Hash(data) == *result.undispatchedRequest
}

func notDispatched(request LaunchRequest) LaunchResult {
	data, err := jsonv2.Marshal(request)
	if err != nil {
		return LaunchResult{}
	}
	digest := framevalue.Hash(data)
	return LaunchResult{undispatchedRequest: &digest}
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

func (o Observation) validate() error {
	if !o.Reference().valid() || o.PID < 0 {
		return frame.ErrInvalid
	}
	switch o.State {
	case Starting:
		if o.PID != 0 || o.ExitCode != nil || o.ProofID != "" {
			return frame.ErrInvalid
		}
	case Running:
		if o.PID == 0 || o.ExitCode != nil || o.ProofID != "" {
			return frame.ErrInvalid
		}
	case NotCreated:
		if o.PID != 0 || o.ExitCode != nil || !frame.ValidName(o.ProofID) {
			return frame.ErrInvalid
		}
	case Exited:
		if o.PID == 0 || o.ExitCode == nil || !frame.ValidName(o.ProofID) {
			return frame.ErrInvalid
		}
	case Unknown:
		if o.ExitCode != nil || o.ProofID != "" {
			return frame.ErrInvalid
		}
	default:
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

type storedAttempt struct {
	Observation   Observation       `json:"observation"`
	RequestDigest framevalue.Digest `json:"requestDigest"`
}

const (
	journalVersion            = 3
	journalFilename           = "attempts.json"
	authorityLockFilename     = "authority.lock"
	lostWaitReason            = "runner restarted without the original child wait identity"
	defaultMaxAttempts        = 4096
	defaultMaxExecutableBytes = 512 << 20
)

type journal struct {
	Version  int                      `json:"version"`
	Epoch    uint64                   `json:"epoch"`
	Attempts map[string]storedAttempt `json:"attempts"`
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

type Server struct {
	config               Config
	id                   string
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
	lock, err := lockDirectory(filepath.Join(config.Directory, authorityLockFilename))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, unlockDirectory(lock))
		}
	}()
	journal, err := loadJournal(filepath.Join(config.Directory, journalFilename), config.MaxAttempts)
	if err != nil {
		return nil, err
	}
	server := &Server{
		config: config, id: rand.Text(), lock: lock, fenced: true, journal: journal,
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
	c.MaxAttempts = cmp.Or(c.MaxAttempts, defaultMaxAttempts)
	c.MaxExecutableBytes = cmp.Or(c.MaxExecutableBytes, defaultMaxExecutableBytes)
	c.MaxControlConnections = cmp.Or(c.MaxControlConnections, defaultControlConnections)
	c.ControlTimeout = cmp.Or(c.ControlTimeout, defaultControlTimeout)
	if c.MaxAttempts < 1 || c.MaxExecutableBytes < 1 || c.MaxExecutableBytes == math.MaxInt64 || c.MaxControlConnections < 1 || c.ControlTimeout < 0 {
		return c, frame.ErrCapacity
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

// loadJournal reads the attempts of an earlier runner. A child it was still
// waiting for becomes Unknown: no later runner holds that wait identity.
func loadJournal(path string, maxAttempts int) (journal, error) {
	loaded := journal{Version: journalVersion, Attempts: make(map[string]storedAttempt)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return loaded, nil
	}
	if err != nil {
		return journal{}, err
	}
	if err := jsonv2.Unmarshal(data, &loaded, jsonv2.RejectUnknownMembers(true)); err != nil {
		return journal{}, fmt.Errorf("runner: invalid journal: %w", err)
	}
	if loaded.Version != journalVersion || loaded.Attempts == nil || len(loaded.Attempts) > maxAttempts {
		return journal{}, errors.New("runner: incompatible journal")
	}
	for id, attempt := range loaded.Attempts {
		if id != attempt.Observation.AttemptID || attempt.Observation.validate() != nil || attempt.RequestDigest.Validate() != nil {
			return journal{}, errors.New("runner: invalid attempt record")
		}
		switch attempt.Observation.State {
		case Starting, Running:
			attempt.Observation.State = Unknown
			attempt.Observation.Reason = lostWaitReason
			attempt.Observation.PID = 0
			loaded.Attempts[id] = attempt
		case Exited, Unknown:
		default:
			return journal{}, errors.New("runner: invalid attempt state")
		}
	}
	return loaded, nil
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

func (s *Server) Launch(ctx context.Context, request LaunchRequest) (_ LaunchResult, err error) {
	if err := request.Validate(); err != nil {
		return notDispatched(request), err
	}
	if err := cancellation.Err(ctx); err != nil {
		return notDispatched(request), err
	}
	digest, err := requestIdentity(request)
	if err != nil {
		return notDispatched(request), err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, found, err := s.replayLaunch(request, digest); err != nil || found {
		return LaunchResult{Observation: previous}, err
	}
	if s.closed || s.fenced || request.RunnerID != s.id || request.Epoch != s.journal.Epoch {
		return notDispatched(request), ErrFenced
	}
	if err := cancellation.Err(ctx); err != nil {
		return notDispatched(request), err
	}
	observation := Observation{AttemptID: request.AttemptID, Epoch: request.Epoch, RunnerID: s.id, State: Starting}
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
			err = errors.Join(err, releaseExecutable(path))
		}
	}()
	if err := cancellation.Err(ctx); err != nil {
		return s.failedStart(request.AttemptID, "launch canceled before process creation", err)
	}
	live, reason, err := spawn(path, request)
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
	s.mu.Lock()
	observation, err := s.inspect(ref.AttemptID)
	if err != nil {
		s.mu.Unlock()
		return observation, err
	}
	if observation.Reference() != ref {
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
			return observation, cancellation.Err(ctx)
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
		cleanupErr = releaseExecutable(path)
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

func releaseExecutable(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("runner: executable snapshot cleanup failed: %w", err)
	}
	return nil
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
	projection := s.journal
	if len(s.terminalObservations) != 0 {
		projection.Attempts = maps.Clone(s.journal.Attempts)
		for id, observation := range s.terminalObservations {
			item := projection.Attempts[id]
			item.Observation = observation
			if observation.State == NotCreated {
				// A refusal must remain replayable even when every active slot is
				// occupied. It uses the same immutable receipt as acknowledged exits.
				if err := s.storeReceipt(item); err != nil {
					return err
				}
				delete(projection.Attempts, id)
			} else {
				projection.Attempts[id] = item
			}
		}
	}
	if err := storeRecord(filepath.Join(s.config.Directory, journalFilename), projection); err != nil {
		return err
	}
	s.journal = projection
	clear(s.terminalObservations)
	return nil
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

func (request LaunchRequest) Validate() error {
	if !request.AttemptRef.valid() || !filepath.IsAbs(request.Command) || !validProgramText(request.Command) || request.ExecutableDigest.Validate() != nil || request.Configuration.Validate() != nil {
		return fmt.Errorf("%w: runner requires exact launch identity", frame.ErrInvalid)
	}
	if request.Directory != "" && (!filepath.IsAbs(request.Directory) || !validProgramText(request.Directory)) {
		return fmt.Errorf("%w: runner working directory must be absolute utf-8 text without nul", frame.ErrInvalid)
	}
	if request.CredentialRevision != "" && !frame.ValidName(request.CredentialRevision) {
		return fmt.Errorf("%w: runner credential revision", frame.ErrInvalid)
	}
	for _, argument := range request.Args {
		if !validProgramText(argument) {
			return fmt.Errorf("%w: runner arguments must be utf-8 text without nul", frame.ErrInvalid)
		}
	}
	names := map[string]bool{}
	for _, entry := range request.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !validProgramText(entry) || names[key] {
			return fmt.Errorf("%w: runner environment requires unique names and utf-8 text without nul", frame.ErrInvalid)
		}
		names[key] = true
	}
	return nil
}

func validProgramText(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsRune(text, '\x00')
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
