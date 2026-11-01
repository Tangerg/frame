// Package process binds Frame's approved execution to a durable local runner
// and the neutral duplex stdio protocol. It never selects a provider.
package process

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
	"github.com/Tangerg/frame/jsonvalue"
	"github.com/Tangerg/frame/runner"
	"github.com/Tangerg/frame/sqlite"
)

// Program is an immutable distribution projection. Secret material belongs in
// Env, never Args or Configuration; CredentialRevision is an opaque revision.
type Program struct {
	Command            string
	ExecutableDigest   integrity.Digest
	Args               []string
	Env                []string
	Directory          string
	CredentialRevision string
	Metadata           map[string]jsonvalue.Value
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
	Attach(context.Context, string) (io.ReadWriteCloser, error)
	Inspect(context.Context, string) (runner.Observation, error)
	CloseAttempt(context.Context, string) (runner.Observation, error)
	AcknowledgeExit(context.Context, runner.ExitReceipt) error
	ProveAbsent(context.Context, runner.AbsenceRequest) (runner.AbsenceProof, error)
}

type Config struct {
	Runner        Runner
	Store         *sqlite.Store
	Source        Source
	CloseTimeout  time.Duration
	MaxFrameBytes int
}

type Driver struct {
	config Config
	mu     sync.Mutex
	live   map[string]*execution
}

type execution struct {
	driver   *Driver
	attempt  sqlite.Attempt
	ready    chan struct{}
	mu       sync.Mutex
	session  *hostSession
	started  bool
	resource *frame.ResourceHandle
	cancel   context.CancelCauseFunc
}

func New(config Config) (*Driver, error) {
	if nilDependency(config.Runner) || config.Store == nil || nilDependency(config.Source) || config.CloseTimeout <= 0 || config.MaxFrameBytes <= 0 || config.MaxFrameBytes > profile.MaxFrameBytes {
		return nil, fmt.Errorf("%w: process driver requires runner, store, source and explicit limits", frame.ErrInvalid)
	}
	return &Driver{config: config, live: make(map[string]*execution)}, nil
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

func (*Driver) Profile() frame.Profile { return frame.LocalProcess }

func (d *Driver) Prepare(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	if setup == nil || !setup.UsesAuthority(d.config.Store) {
		return nil, frame.ErrFenced
	}
	unit := setup.Unit()
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
	assembly, err := assemble(setup, program, d.config.MaxFrameBytes)
	if err != nil {
		return nil, err
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
	attempt := sqlite.Attempt{ID: setup.InstanceID(), Epoch: setup.Epoch(), Source: setup.Binding().Source, Binding: setup.Snapshot(), Realm: identity.ID, Profile: frame.LocalProcess}
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
		proof := sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: attempt.ID + ":not-dispatched", Kind: "not-dispatched", Evidence: []byte(`{"dispatched":false}`)}
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
		AttemptID: attempt.ID, Epoch: attempt.Epoch, Command: program.Command,
		ExecutableDigest: program.ExecutableDigest, Args: program.Args, Env: program.Env,
		Directory: program.Directory, Configuration: setup.Binding().Configuration,
		CredentialRevision: program.CredentialRevision,
	})
	if err != nil {
		return nil, err
	}
	if observation.RunnerID != attempt.Realm || observation.State != runner.Running {
		return nil, runner.ErrUnknown
	}
	attached, err := d.config.Runner.Attach(ownedCtx, attempt.ID)
	if err != nil {
		return nil, err
	}
	values, err := assembly.connect(ownedCtx, attached, setup)
	e.mu.Lock()
	e.session = assembly.session
	e.mu.Unlock()
	return values, err
}

func (e *execution) close(ctx context.Context) (frame.ReleaseState, error) {
	e.cancel(frame.ErrClosed)
	ctx, cancel := context.WithTimeout(ctx, e.driver.config.CloseTimeout)
	defer cancel()
	select {
	case <-e.ready:
	case <-ctx.Done():
		return frame.ResourceUnconfirmed, context.Cause(ctx)
	}
	e.mu.Lock()
	session, started := e.session, e.started
	e.session = nil
	e.mu.Unlock()
	var transportErr error
	if session != nil {
		transportErr = session.close(ctx)
	}
	if !started {
		proof := sqlite.ExitProof{AttemptID: e.attempt.ID, Realm: e.attempt.Realm, ExitID: e.attempt.ID + ":not-dispatched", Kind: "not-dispatched", Evidence: []byte(`{"dispatched":false}`)}
		if err := e.driver.config.Store.ReleaseAttempt(ctx, e.attempt.ID, proof); err != nil {
			return frame.ResourceUnconfirmed, errors.Join(transportErr, err)
		}
		e.driver.forget(e.attempt.ID)
		return frame.ResourceReleased, transportErr
	}
	observed, err := e.driver.config.Runner.CloseAttempt(ctx, e.attempt.ID)
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
	proof := sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: observed.ProofID, Kind: "runner-wait", Evidence: evidence}
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

// Reconcile consumes late evidence for the original resource. It never repeats
// cleanup or launches a replacement under that attempt identity.
func (d *Driver) Reconcile(ctx context.Context, id string) error {
	d.mu.Lock()
	e := d.live[id]
	d.mu.Unlock()
	if e == nil {
		return frame.ErrUnavailable
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
	records, err := d.config.Store.Attempts(ctx)
	if err != nil {
		return err
	}
	pending := false
	for _, record := range records {
		if (!record.Released || record.PendingHandoff) && record.Attempt.Profile == frame.LocalProcess {
			pending = true
			break
		}
	}
	if !pending {
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
	for _, record := range records {
		if record.Attempt.Profile == frame.LocalProcess {
			failures = errors.Join(failures, d.recoverAttempt(ctx, identity, record))
		}
	}
	return failures
}

func (d *Driver) recoverAttempt(ctx context.Context, identity runner.Identity, record sqlite.AttemptRecord) error {
	attempt := record.Attempt
	if record.PendingHandoff {
		return d.acknowledgeExit(ctx, attempt, *record.Proof)
	}
	if record.Released {
		return nil
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
	observed, err := d.config.Runner.CloseAttempt(ctx, attempt.ID)
	if errors.Is(err, runner.ErrNotFound) {
		return d.releaseAbsentAttempt(ctx, attempt)
	}
	if err != nil {
		return err
	}
	return d.release(ctx, attempt, observed)
}

func (d *Driver) releaseAbsentAttempt(ctx context.Context, attempt sqlite.Attempt) error {
	request := runner.AbsenceRequest{AttemptID: attempt.ID, Epoch: attempt.Epoch, RunnerID: attempt.Realm}
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
	return d.config.Store.ReleaseAttempt(ctx, attempt.ID, sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: proof.ProofID, Kind: "sealed-not-started", Evidence: evidence})
}
