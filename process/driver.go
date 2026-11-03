// Package process binds Frame's approved execution to a durable local runner
// and a selected stdio protocol. It never selects a provider.
package process

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/Tangerg/frame"
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
	Launch(context.Context, runner.LaunchRequest) (runner.Observation, error)
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
	ready    chan struct{}
	mu       sync.Mutex
	cleanup  cleanupPhase
	session  Connection
	channel  *attachment
	started  bool
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
	if nilDependency(config.Runner) || config.Store == nil || nilDependency(config.Source) || config.CloseTimeout <= 0 || config.MaxFrameBytes <= 0 {
		return nil, fmt.Errorf("%w: process driver requires runner, store, source and explicit limits", frame.ErrInvalid)
	}
	if config.Protocol == nil {
		config.Protocol = localProtocol{}
	}
	if nilDependency(config.Protocol) {
		return nil, fmt.Errorf("%w: missing process protocol", frame.ErrInvalid)
	}
	name := config.Protocol.Profile()
	if err := name.Validate(); err != nil {
		return nil, err
	}
	if name == frame.LocalProcess && config.MaxFrameBytes > profile.MaxFrameBytes {
		return nil, fmt.Errorf("%w: neutral process frame exceeds %d bytes", frame.ErrInvalid, profile.MaxFrameBytes)
	}
	if name == frame.NativeGo {
		return nil, fmt.Errorf("%w: reserved native profile", frame.ErrInvalid)
	}
	return &Driver{config: config, profile: name, live: make(map[string]*execution)}, nil
}

func nilDependency(dependency any) bool {
	value := reflect.ValueOf(dependency)
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return value.IsNil()
	default:
		return false
	}
}

func (d *Driver) Profile() frame.Profile { return d.profile }

func (d *Driver) Prepare(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	if setup == nil || !setup.UsesAuthority(d.config.Store) {
		return nil, frame.ErrFenced
	}
	unit := setup.Unit()
	if unit.Profile != d.profile {
		return nil, frame.ErrContractMismatch
	}
	for _, requirement := range unit.Imports {
		if requirement.Contract.Mode != frame.PortableMode {
			return nil, frame.ErrContractMismatch
		}
	}
	for _, export := range unit.Exports {
		if export.Contract.Mode != frame.PortableMode {
			return nil, frame.ErrContractMismatch
		}
	}
	program, err := d.config.Source.Program(ctx, setup.Binding())
	var content *frame.ResourceHandle
	if program.ReleaseContent != nil {
		var ownErr error
		content, ownErr = setup.Cleanup("process content", program.ReleaseContent)
		if ownErr != nil {
			return nil, errors.Join(err, ownErr, program.ReleaseContent())
		}
	}
	if err != nil {
		return nil, err
	}
	connection, err := d.config.Protocol.Prepare(setup, program.Metadata, d.config.MaxFrameBytes)
	if err != nil {
		return nil, err
	}
	if nilDependency(connection) {
		return nil, fmt.Errorf("%w: missing protocol connection", frame.ErrInvalid)
	}
	if err := d.config.Runner.Fence(ctx, setup.Epoch()); err != nil {
		return nil, err
	}
	identity, err := d.config.Runner.Identity(ctx)
	if err != nil {
		return nil, err
	}
	if identity.Fenced || identity.Epoch != setup.Epoch() {
		return nil, frame.ErrFenced
	}
	attempt := sqlite.Attempt{ID: setup.InstanceID(), Epoch: setup.Epoch(), Source: setup.Binding().Source, Binding: setup.Snapshot(), Realm: identity.ID, Profile: d.profile}
	if err := d.config.Store.RecordAttempt(ctx, attempt); err != nil {
		return nil, err
	}
	ownedCtx, cancel := context.WithCancelCause(setup.Lifetime().Context())
	stopPreparation := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	defer stopPreparation()
	e := &execution{driver: d, attempt: attempt, ready: make(chan struct{}), cancel: cancel}
	var resources []*frame.ResourceHandle
	if content != nil {
		resources = append(resources, content)
	}
	resource, err := setup.Own(frame.Resource{Name: "local process " + attempt.ID, Cancel: cancel, Close: e.close}, resources...)
	if err != nil {
		cancel(err)
		// No creation was dispatched; this proof is local, exact and final.
		proof := undispatchedProof(attempt)
		return nil, errors.Join(err, d.config.Store.ReleaseAttempt(context.WithoutCancel(ctx), attempt.ID, proof))
	}
	e.resource = resource
	d.mu.Lock()
	d.live[attempt.ID] = e
	d.mu.Unlock()
	defer close(e.ready)
	if err := ownedCtx.Err(); err != nil {
		return nil, errors.Join(err, context.Cause(ownedCtx))
	}
	e.mu.Lock()
	e.started = true
	e.mu.Unlock()
	observation, err := d.config.Runner.Launch(ownedCtx, runner.LaunchRequest{
		AttemptRef: runnerAttempt(attempt), Command: program.Command,
		ExecutableDigest: program.ExecutableDigest, Args: program.Args, Env: program.Env,
		Directory: program.Directory, Configuration: setup.Binding().Configuration,
		CredentialRevision: program.CredentialRevision,
	})
	if err != nil {
		return nil, err
	}
	if observation.Reference() != runnerAttempt(attempt) || observation.State != runner.Running {
		return nil, runner.ErrUnknown
	}
	attached, err := d.config.Runner.Attach(ownedCtx, runnerAttempt(attempt))
	if err != nil {
		return nil, err
	}
	channel := &attachment{ReadWriteCloser: attached}
	e.mu.Lock()
	e.channel, e.session = channel, connection
	e.mu.Unlock()
	stopInterrupt := context.AfterFunc(ownedCtx, func() { _ = channel.Close() })
	defer stopInterrupt()
	return connection.Connect(ownedCtx, channel)
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
	session, channel, started := e.session, e.channel, e.started
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
	if !started {
		proof := undispatchedProof(e.attempt)
		if err := e.driver.config.Store.ReleaseAttempt(ctx, e.attempt.ID, proof); err != nil {
			return frame.ResourceUnconfirmed, errors.Join(transportErr, err)
		}
		e.driver.forget(e.attempt.ID)
		return frame.ResourceReleased, transportErr
	}
	observed, err := e.driver.config.Runner.CloseAttempt(ctx, runnerAttempt(e.attempt))
	if err != nil {
		return frame.ResourceUnconfirmed, errors.Join(transportErr, err)
	}
	if err := e.driver.release(ctx, e.attempt, observed); err != nil {
		return frame.ResourceUnconfirmed, errors.Join(transportErr, err)
	}
	e.driver.forget(e.attempt.ID)
	return frame.ResourceReleased, transportErr
}

func (d *Driver) forget(id string) {
	d.mu.Lock()
	delete(d.live, id)
	d.mu.Unlock()
}

func (d *Driver) release(ctx context.Context, attempt sqlite.Attempt, observed runner.Observation) error {
	if observed.AttemptID != attempt.ID || observed.Epoch != attempt.Epoch || observed.RunnerID != attempt.Realm || observed.State != runner.Exited || observed.ProofID == "" {
		return runner.ErrUnknown
	}
	evidence, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	proof := sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: observed.ProofID, Kind: runnerWaitProofKind, Evidence: evidence}
	if err := d.config.Store.AcceptExit(ctx, attempt.ID, proof); err != nil {
		return err
	}
	return d.acknowledgeExit(ctx, attempt, proof)
}

func (d *Driver) acknowledgeExit(ctx context.Context, attempt sqlite.Attempt, proof sqlite.ExitProof) error {
	receipt := runner.ExitReceipt{AttemptID: attempt.ID, Epoch: attempt.Epoch, RunnerID: attempt.Realm, ProofID: proof.ExitID}
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
	observed, err := d.config.Runner.Inspect(ctx, id)
	if err != nil {
		return err
	}
	if err := d.release(ctx, e.attempt, observed); err != nil {
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
	if record.PendingHandoff {
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
	evidence, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	return d.config.Store.ReleaseAttempt(ctx, attempt.ID, sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: proof.ProofID, Kind: sealedAbsenceProofKind, Evidence: evidence})
}
