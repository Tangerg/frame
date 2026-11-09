// Package process binds Frame's approved execution to a durable local runner
// and a selected stdio protocol. It never selects a provider.
package process

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/internal/nilness"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	framevalue "github.com/Tangerg/frame/value"
)

// Program is an immutable distribution projection. Secret material belongs in
// Env, never Args or Configuration; CredentialRevision is an opaque revision.
type Program struct {
	Command            string
	ExecutableDigest   framevalue.Digest
	Args               []string
	Env                []string
	Directory          string
	CredentialRevision string
	Metadata           ExportMetadata
	// ReleaseContent transfers an acquired immutable-content lease to the instance.
	ReleaseContent func() error
}

type Source interface {
	Program(context.Context, frame.UnitBinding) (Program, error)
}

// Runner is the original process authority; a consumer may start and connect its
// independent daemon before delegating these protocol operations.
type Runner interface {
	Identity(context.Context) (runner.Identity, error)
	Fence(context.Context, uint64) error
	Launch(context.Context, runner.LaunchRequest) (runner.LaunchResult, error)
	Attach(context.Context, runner.AttemptRef) (io.ReadWriteCloser, error)
	Inspect(context.Context, string) (runner.Observation, error)
	CloseAttempt(context.Context, runner.AttemptRef) (runner.Observation, error)
	AcknowledgeExit(context.Context, runner.ExitReceipt) error
	ProveAbsent(context.Context, runner.AttemptRef) (runner.AbsenceProof, error)
}

type Config struct {
	Runner        Runner
	Store         *sqlite.Store
	Source        Source
	CloseTimeout  time.Duration
	MaxFrameBytes int
	Protocol      Protocol
}

type Driver struct {
	config  Config
	profile frame.Profile
	mu      sync.Mutex
	live    map[string]*execution
}

type execution struct {
	driver   *Driver
	attempt  sqlite.Attempt
	request  runner.LaunchRequest
	ready    chan struct{}
	mu       sync.Mutex
	cleanup  cleanupPhase
	session  Connection
	channel  *attachment
	launch   *runner.LaunchResult
	resource *frame.ResourceHandle
	cancel   context.CancelCauseFunc
}

type cleanupPhase uint8

const (
	cleanupNotStarted cleanupPhase = iota
	cleanupInProgress
	cleanupFinished
)

func New(config Config) (*Driver, error) {
	if nilness.Is(config.Runner) || config.Store == nil || nilness.Is(config.Source) || config.CloseTimeout <= 0 || config.MaxFrameBytes <= 0 {
		return nil, fmt.Errorf("%w: process driver requires runner, store, source and explicit limits", frame.ErrInvalid)
	}
	if config.Protocol == nil {
		config.Protocol = localProtocol{}
	}
	if nilness.Is(config.Protocol) {
		return nil, fmt.Errorf("%w: missing process protocol", frame.ErrInvalid)
	}
	name := config.Protocol.Profile()
	if err := name.Validate(); err != nil {
		return nil, err
	}
	if name == frame.LocalProcess && config.MaxFrameBytes > wire.MaxFrameBytes {
		return nil, fmt.Errorf("%w: neutral process frame exceeds %d bytes", frame.ErrInvalid, wire.MaxFrameBytes)
	}
	if name == frame.NativeGo {
		return nil, fmt.Errorf("%w: reserved native profile", frame.ErrInvalid)
	}
	return &Driver{config: config, profile: name, live: make(map[string]*execution)}, nil
}

func (d *Driver) Profile() frame.Profile { return d.profile }

const (
	executionResourcePrefix = "local process "
	contentResourceName     = "process content"
)

func (d *Driver) Prepare(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	if setup == nil || !setup.UsesAuthority(d.config.Store) {
		return nil, frame.ErrFenced
	}
	if err := d.checkUnit(setup.Unit()); err != nil {
		return nil, err
	}
	program, content, err := d.program(ctx, setup)
	if err != nil {
		return nil, err
	}
	connection, err := d.config.Protocol.Prepare(setup, program.Metadata, d.config.MaxFrameBytes)
	if err != nil {
		return nil, err
	}
	if nilness.Is(connection) {
		return nil, fmt.Errorf("%w: missing protocol connection", frame.ErrInvalid)
	}
	realm, err := d.currentRunner(ctx, setup.Epoch())
	if err != nil {
		return nil, err
	}
	attempt := sqlite.Attempt{ID: setup.InstanceID(), Epoch: setup.Epoch(), Source: setup.Binding().Source, Binding: setup.Snapshot(), Realm: realm, Profile: d.profile}
	request := runner.LaunchRequest{
		AttemptRef: runnerAttempt(attempt), Command: program.Command,
		ExecutableDigest: program.ExecutableDigest, Args: slices.Clone(program.Args), Env: slices.Clone(program.Env),
		Directory: program.Directory, Configuration: setup.Binding().Configuration,
		CredentialRevision: program.CredentialRevision,
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if err := d.checkExitCapacity(attempt); err != nil {
		return nil, err
	}
	if err := d.config.Store.RecordAttempt(ctx, attempt); err != nil {
		return nil, err
	}
	ownedCtx, cancel := context.WithCancelCause(setup.Lifetime().Context())
	stopPreparation := context.AfterFunc(ctx, func() { cancel(cancellation.Err(ctx)) })
	defer stopPreparation()
	e := &execution{driver: d, attempt: attempt, request: request, ready: make(chan struct{}), cancel: cancel}
	var dependencies []*frame.ResourceHandle
	if content != nil {
		dependencies = append(dependencies, content)
	}
	resource, err := setup.Own(frame.Resource{Name: executionResourcePrefix + attempt.ID, Cancel: cancel, Close: e.close}, dependencies...)
	if err != nil {
		cancel(err)
		// No creation was dispatched; this proof is local, exact and final.
		return nil, errors.Join(err, d.config.Store.ReleaseAttempt(context.WithoutCancel(ctx), attempt.ID, undispatchedProof(attempt)))
	}
	e.resource = resource
	d.mu.Lock()
	d.live[attempt.ID] = e
	d.mu.Unlock()
	defer close(e.ready)
	if err := cancellation.Err(ownedCtx); err != nil {
		return nil, err
	}
	return e.start(ownedCtx, connection)
}

// checkUnit admits only Portable contracts: a child process cannot share a
// native Go value with the Host.
func (d *Driver) checkUnit(unit frame.UnitDeclaration) error {
	if unit.Profile != d.profile {
		return frame.ErrContractMismatch
	}
	for _, requirement := range unit.Imports {
		if requirement.Contract.Mode != frame.PortableMode {
			return frame.ErrContractMismatch
		}
	}
	for _, export := range unit.Exports {
		if export.Contract.Mode != frame.PortableMode {
			return frame.ErrContractMismatch
		}
	}
	return nil
}

func (d *Driver) program(ctx context.Context, setup *frame.Setup) (Program, *frame.ResourceHandle, error) {
	var program Program
	// Preparation work keeps cleanup behind the original acquisition, including
	// a content lease returned together with a resolution error.
	content, err := setup.Cleanup(contentResourceName, func() error {
		if program.ReleaseContent == nil {
			return nil
		}
		return program.ReleaseContent()
	})
	if err != nil {
		return Program{}, nil, err
	}
	program, err = d.config.Source.Program(ctx, setup.Binding())
	if program.ReleaseContent == nil {
		return program, nil, errors.Join(err, content.ConfirmReleased(nil))
	}
	return program, content, err
}

// currentRunner fences older epochs and returns the identity of the runner
// that will own the child, which must already serve this epoch.
func (d *Driver) currentRunner(ctx context.Context, epoch uint64) (string, error) {
	if err := d.config.Runner.Fence(ctx, epoch); err != nil {
		return "", err
	}
	identity, err := d.config.Runner.Identity(ctx)
	if err != nil {
		return "", err
	}
	if identity.Fenced || identity.Epoch != epoch {
		return "", frame.ErrFenced
	}
	return identity.ID, nil
}

func (e *execution) start(ctx context.Context, connection Connection) ([]frame.ExportValue, error) {
	e.mu.Lock()
	// A zero result is unknown, never permission to release. Keep this original
	// invocation: an undispatched retry could not negate an earlier acceptance.
	e.launch = new(runner.LaunchResult)
	e.mu.Unlock()
	attempt := runnerAttempt(e.attempt)
	result, err := e.driver.config.Runner.Launch(ctx, e.request)
	e.mu.Lock()
	*e.launch = result
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if result.Reference() != attempt || result.State != runner.Running {
		return nil, runner.ErrUnknown
	}
	attached, err := e.driver.config.Runner.Attach(ctx, attempt)
	if err != nil {
		return nil, err
	}
	channel := &attachment{ReadWriteCloser: attached}
	e.mu.Lock()
	e.channel, e.session = channel, connection
	e.mu.Unlock()
	stopInterrupt := context.AfterFunc(ctx, func() { _ = channel.Close() })
	defer stopInterrupt()
	return connection.Connect(ctx, channel)
}

func (e *execution) close(ctx context.Context) (frame.ReleaseState, error) {
	e.cancel(frame.ErrClosed)
	e.mu.Lock()
	e.cleanup = cleanupInProgress
	e.mu.Unlock()
	<-e.ready
	ctx, cancel := context.WithTimeout(ctx, e.driver.config.CloseTimeout)
	defer cancel()
	e.mu.Lock()
	session, channel := e.session, e.channel
	e.session, e.channel = nil, nil
	e.mu.Unlock()
	var transportErr error
	if session != nil {
		stopInterrupt := context.AfterFunc(ctx, func() { _ = channel.Close() })
		transportErr = session.Close(ctx)
		stopInterrupt()
	}
	if channel != nil {
		transportErr = errors.Join(transportErr, channel.Close())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cleanup = cleanupFinished
	if err := e.settle(ctx); err != nil {
		return frame.ResourceUnconfirmed, errors.Join(transportErr, err)
	}
	e.driver.forget(e.attempt.ID)
	return frame.ResourceReleased, transportErr
}

func (e *execution) settle(ctx context.Context) error {
	if e.launch == nil || e.launch.NotDispatched(e.request) {
		return e.driver.config.Store.ReleaseAttempt(ctx, e.attempt.ID, undispatchedProof(e.attempt))
	}
	observed, err := e.driver.config.Runner.CloseAttempt(ctx, runnerAttempt(e.attempt))
	if err != nil {
		return err
	}
	return e.driver.release(ctx, e.attempt, observed)
}

func (d *Driver) forget(id string) {
	d.mu.Lock()
	delete(d.live, id)
	d.mu.Unlock()
}

func (d *Driver) release(ctx context.Context, attempt sqlite.Attempt, observed runner.Observation) error {
	if observed.Reference() != runnerAttempt(attempt) {
		return runner.ErrUnknown
	}
	receipt, err := observed.ExitReceipt()
	if err != nil {
		return err
	}
	proof, err := runnerExitProof(attempt, receipt)
	if err != nil {
		return err
	}
	if err := d.config.Store.AcceptExit(ctx, attempt.ID, proof); err != nil {
		return err
	}
	return d.acknowledgeExit(ctx, attempt, proof)
}

func (d *Driver) acknowledgeExit(ctx context.Context, attempt sqlite.Attempt, proof sqlite.ExitProof) error {
	var receipt runner.ExitReceipt
	if err := json.Unmarshal(proof.Evidence, &receipt, json.RejectUnknownMembers(true)); err != nil {
		return errors.Join(runner.ErrUnknown, err)
	}
	if proof.Kind != runnerProofKind(receipt.State) || receipt != (runner.ExitReceipt{AttemptID: attempt.ID, Epoch: attempt.Epoch, RunnerID: attempt.Realm, ProofID: proof.ExitID, State: receipt.State}) || (receipt.State != runner.Exited && receipt.State != runner.NotCreated) {
		return runner.ErrUnknown
	}
	if err := d.config.Runner.AcknowledgeExit(ctx, receipt); err != nil {
		return err
	}
	return d.config.Store.ReleaseAttempt(ctx, attempt.ID, proof)
}

// A child's exit cannot prove that its protocol's host-side work has stopped.
// Reconcile accepts late evidence only after the original local cleanup completes.
func (d *Driver) Reconcile(ctx context.Context, id string) error {
	d.mu.Lock()
	e := d.live[id]
	d.mu.Unlock()
	if e == nil {
		return frame.ErrUnavailable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cleanup != cleanupFinished {
		return fmt.Errorf("%w: process reconciliation requires completed local cleanup", frame.ErrInvalid)
	}
	if err := e.settle(ctx); err != nil {
		return err
	}
	if err := e.resource.ConfirmReleased(nil); err != nil {
		return err
	}
	d.forget(id)
	return nil
}

// Recover seals old creation before examining absence. Only the same live
// runner can prove an old attempt did not start or has actually exited.
func (d *Driver) Recover(ctx context.Context) error {
	records, err := d.config.Store.PendingAttempts(ctx)
	if err != nil {
		return err
	}
	pending := slices.DeleteFunc(records, func(record sqlite.AttemptRecord) bool {
		return record.Attempt.Profile != d.profile
	})
	if len(pending) == 0 {
		return nil
	}
	identity, err := d.config.Runner.Identity(ctx)
	if err != nil {
		return err
	}
	if err := d.config.Runner.Fence(ctx, d.config.Store.Epoch()); err != nil {
		return err
	}
	var failures error
	for _, record := range pending {
		failures = errors.Join(failures, d.recoverAttempt(ctx, identity, record))
	}
	return failures
}

func (d *Driver) recoverAttempt(ctx context.Context, identity runner.Identity, record sqlite.AttemptRecord) error {
	attempt := record.Attempt
	if record.State == sqlite.AttemptHandoff {
		return d.acknowledgeExit(ctx, attempt, *record.Proof)
	}
	if attempt.Epoch >= d.config.Store.Epoch() {
		return runner.ErrUnknown
	}
	if attempt.Realm != identity.ID {
		// A replacement authority may replay the original durable exit but
		// cannot stop a surviving process whose wait identity it never owned.
		observed, err := d.config.Runner.Inspect(ctx, attempt.ID)
		if err == nil {
			err = d.release(ctx, attempt, observed)
		}
		if err != nil {
			return errors.Join(runner.ErrUnknown, err)
		}
		return nil
	}
	observed, err := d.config.Runner.CloseAttempt(ctx, runnerAttempt(attempt))
	if errors.Is(err, runner.ErrNotFound) {
		return d.releaseAbsentAttempt(ctx, attempt)
	}
	if err != nil {
		return err
	}
	return d.release(ctx, attempt, observed)
}

func runnerAttempt(attempt sqlite.Attempt) runner.AttemptRef {
	return runner.AttemptRef{AttemptID: attempt.ID, Epoch: attempt.Epoch, RunnerID: attempt.Realm}
}

func (d *Driver) releaseAbsentAttempt(ctx context.Context, attempt sqlite.Attempt) error {
	request := runnerAttempt(attempt)
	proof, err := d.config.Runner.ProveAbsent(ctx, request)
	if err == nil {
		err = proof.Validate(request)
	}
	if err != nil {
		return errors.Join(runner.ErrUnknown, err)
	}
	exit, err := absenceExitProof(attempt, proof)
	if err != nil {
		return err
	}
	return d.config.Store.ReleaseAttempt(ctx, attempt.ID, exit)
}
