// Package process binds Frame's approved execution to a durable local runner
// and a selected stdio protocol. It never selects a provider.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
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
	Program(context.Context, framehost.UnitBinding) (Program, error)
}

// Runner is the original process authority; a consumer may start and connect its
// independent daemon before delegating these protocol operations.
type Runner interface {
	Identity(context.Context) (runner.Identity, error)
	Fence(context.Context, uint64) error
	// Launch given a done context must not dispatch: it returns the request's
	// non-dispatch proof. The Driver relies on this while creation is queued.
	Launch(context.Context, runner.LaunchRequest) (runner.LaunchResult, error)
	Attach(context.Context, runner.AttemptRef) (io.ReadWriteCloser, error)
	Inspect(context.Context, string) (runner.Observation, error)
	CloseAttempt(context.Context, runner.AttemptRef) (runner.Observation, error)
	AcknowledgeExit(context.Context, runner.ExitReceipt) error
	ProveAbsent(context.Context, runner.AttemptRef) (runner.AbsenceProof, error)
}

type Config struct {
	Runner        Runner
	Store         sqlite.Store
	Source        Source
	CloseTimeout  time.Duration
	MaxFrameBytes int
	Protocol      Protocol
}

type Driver struct {
	config  Config
	profile framehost.Profile
	// creating admits one creation at a time. The runner serializes creation
	// in its journal, where a queued request can no longer be withdrawn;
	// waiting here instead keeps cancellation a proof of non-dispatch.
	creating chan struct{}
	mu       sync.Mutex
	live     map[string]*execution
}

type execution struct {
	driver  *Driver
	attempt sqlite.Attempt
	request runner.LaunchRequest
	ready   chan struct{}
	// Preparation writes this conclusion until ready closes. Retirement and
	// reconciliation cannot read it before that original preparation has ended.
	approvalCommit framehost.CommitState
	mu             sync.Mutex
	// cleanedUp records that the original local cleanup returned; only then
	// may Reconcile accept late exit evidence.
	cleanedUp bool
	session   Connection
	channel   *attachment
	launch    *runner.LaunchResult
	resource  frame.ResourceHandle
	cancel    context.CancelCauseFunc
}

func New(config Config) (*Driver, error) {
	if framevalue.IsNil(config.Runner) || !config.Store.Valid() || framevalue.IsNil(config.Source) || config.CloseTimeout <= 0 || config.MaxFrameBytes <= 0 {
		return nil, fmt.Errorf("%w: process driver requires runner, store, source and explicit limits", frame.ErrInvalid)
	}
	if config.Protocol == nil {
		local := localProtocol{}
		if err := local.admitFrameLimit(config.MaxFrameBytes); err != nil {
			return nil, err
		}
		config.Protocol = local
	}
	if framevalue.IsNil(config.Protocol) {
		return nil, fmt.Errorf("%w: missing process protocol", frame.ErrInvalid)
	}
	name := config.Protocol.Profile()
	if err := name.Validate(); err != nil {
		return nil, err
	}
	if name == framehost.NativeGo {
		return nil, fmt.Errorf("%w: reserved native profile", frame.ErrInvalid)
	}
	return &Driver{config: config, profile: name, creating: make(chan struct{}, 1), live: make(map[string]*execution)}, nil
}

func (d *Driver) Profile() framehost.Profile { return d.profile }

const (
	executionResourcePrefix = "local process "
	contentResourceName     = "process content"
)

func (d *Driver) Prepare(ctx context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
	if setup.Durable() != framehost.Durable(d.config.Store) {
		return nil, framehost.ErrFenced
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
	if framevalue.IsNil(connection) {
		return nil, fmt.Errorf("%w: missing protocol connection", frame.ErrInvalid)
	}
	realm, err := d.currentRunner(ctx, setup.Epoch())
	if err != nil {
		return nil, err
	}
	attempt := sqlite.NewAttempt(setup, realm)
	request := runner.LaunchRequest{
		AttemptRef: runnerAttempt(attempt), Command: program.Command,
		ExecutableDigest: program.ExecutableDigest, Args: slices.Clone(program.Args), Env: slices.Clone(program.Env),
		Directory: program.Directory, Configuration: setup.Binding().Configuration,
		CredentialRevision: program.CredentialRevision,
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	ownedCtx, cancel := context.WithCancelCause(setup.Lifetime().Context())
	stopPreparation := context.AfterFunc(ctx, func() { cancel(frame.ContextError(ctx)) })
	defer stopPreparation()
	e := &execution{driver: d, attempt: attempt, request: request, ready: make(chan struct{}), cancel: cancel, approvalCommit: framehost.NotCommitted}
	var dependencies []frame.ResourceHandle
	if content.Valid() {
		dependencies = append(dependencies, content)
	}
	resource, err := setup.Own(frame.Resource{Name: executionResourcePrefix + attempt.ID, Cancel: cancel, Close: e.close}, dependencies...)
	if err != nil {
		cancel(err)
		return nil, err
	}
	e.resource = resource
	d.mu.Lock()
	d.live[attempt.ID] = e
	d.mu.Unlock()
	defer close(e.ready)
	e.approvalCommit = framehost.CommitIndeterminate
	state, err := d.config.Store.RecordAttempt(ctx, attempt)
	if state == framehost.Committed || state == framehost.NotCommitted {
		e.approvalCommit = state
	}
	if err != nil {
		return nil, err
	}
	if state != framehost.Committed {
		return nil, framehost.ErrRecoveryRequired
	}
	if err := frame.ContextError(ownedCtx); err != nil {
		return nil, err
	}
	return e.start(ownedCtx, connection)
}

func (d *Driver) checkUnit(unit framehost.UnitDeclaration) error {
	if unit.Profile != d.profile {
		return frame.ErrContractMismatch
	}
	return unit.RequirePortable()
}

func (d *Driver) program(ctx context.Context, setup framehost.Setup) (Program, frame.ResourceHandle, error) {
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
		return Program{}, frame.ResourceHandle{}, err
	}
	program, err = d.config.Source.Program(ctx, setup.Binding())
	if program.ReleaseContent == nil {
		return program, frame.ResourceHandle{}, errors.Join(err, content.ConfirmReleased(nil))
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
		return "", framehost.ErrFenced
	}
	return identity.ID, nil
}

func (e *execution) start(ctx context.Context, connection Connection) ([]framehost.ExportValue, error) {
	e.mu.Lock()
	// A zero result is unknown, never permission to release. Keep this original
	// invocation: an undispatched retry could not negate an earlier acceptance.
	e.launch = new(runner.LaunchResult)
	e.mu.Unlock()
	attempt := runnerAttempt(e.attempt)
	result, err := e.driver.launch(ctx, e.request)
	e.mu.Lock()
	*e.launch = result
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if result.AttemptRef != attempt || result.State != runner.Running {
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

// launch dispatches a durably admitted creation. Cancellation before dispatch
// keeps the runner's exact non-dispatch proof. Once dispatched, the original
// request is joined: cancellation could otherwise discard its acknowledgement
// and turn an ordinary close into an unknown creation.
func (d *Driver) launch(ctx context.Context, request runner.LaunchRequest) (runner.LaunchResult, error) {
	select {
	case d.creating <- struct{}{}:
		defer func() { <-d.creating }()
	case <-ctx.Done():
		return d.config.Runner.Launch(ctx, request)
	}
	if ctx.Err() != nil {
		return d.config.Runner.Launch(ctx, request)
	}
	return d.config.Runner.Launch(context.WithoutCancel(ctx), request)
}

func (e *execution) close(ctx context.Context) (frame.ReleaseState, error) {
	e.cancel(frame.ErrClosed)
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
	e.cleanedUp = true
	if err := e.settle(ctx); err != nil {
		return frame.ResourceUnconfirmed, errors.Join(transportErr, err)
	}
	e.driver.forget(e.attempt.ID)
	return frame.ResourceReleased, transportErr
}

func (e *execution) settle(ctx context.Context) error {
	if e.approvalCommit == framehost.NotCommitted {
		return nil
	}
	if e.launch == nil || e.launch.NotDispatched(e.request) {
		if e.approvalCommit == framehost.CommitIndeterminate {
			return e.driver.config.Store.SettleAttempt(ctx, e.attempt, undispatchedProof(e.attempt))
		}
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
	if observed.AttemptRef != runnerAttempt(attempt) {
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
	if err := framevalue.DecodeValue(proof.Evidence, &receipt, "runner exit receipt"); err != nil {
		return errors.Join(runner.ErrUnknown, err)
	}
	if err := receipt.Validate(); err != nil {
		return errors.Join(runner.ErrUnknown, err)
	}
	if proof.Kind != runnerExitProofKind || receipt.AttemptRef != runnerAttempt(attempt) || receipt.ProofID != proof.ExitID {
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
		return framehost.ErrUnavailable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cleanedUp {
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
