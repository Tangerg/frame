package host

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type InstanceState string

const (
	Preparing   InstanceState = "preparing"
	Ready       InstanceState = "ready"
	Draining    InstanceState = "draining"
	Closing     InstanceState = "closing"
	CloseFailed InstanceState = "closeFailed"
	Closed      InstanceState = "closed"
)

type InstanceObservation struct {
	ID         string
	Generation uint64
	Source     UnitRef
	State      InstanceState
	Failure    error
	Close      *frame.CloseOutcome
}

type instanceKey struct {
	source UnitRef
	realm  RealmID
}

// One Host worker closes after preparation and consumers drain. Resource
// callbacks observe that original exit instead of starting another drain.
type instanceRetirement struct {
	once    sync.Once
	exit    frame.Close
	closing chan struct{}
	done    chan struct{}
	causes  *frame.CloseCauses
}

func newInstanceRetirement(lifetime frame.Lifetime) instanceRetirement {
	return instanceRetirement{closing: make(chan struct{}), done: make(chan struct{}), causes: lifetime.ObserveCloseCauses()}
}

func (r *instanceRetirement) publishExit(exit frame.Close) {
	r.exit = exit
	close(r.closing)
}

func (r *instanceRetirement) awaitExit(ctx context.Context) (frame.CloseOutcome, error) {
	if err := awaitRetired(ctx, r.closing); err != nil {
		return frame.CloseOutcome{}, err
	}
	return r.exit.Wait(ctx)
}

// The hold retains this exact instance and its provider edges until release.
// Keeping both together prevents a dependency index from naming an unheld instance.
// Cancellation follows the instance context; observing retention instead can
// wait on the consumer that cancellation must release.
type instanceHold struct {
	instance *unitInstance
	work     frame.Work
}

type heldInstances []instanceHold

func (holds heldInstances) closure() iter.Seq[*unitInstance] {
	return func(yield func(*unitInstance) bool) {
		pending := slices.Clone(holds)
		visited := make(map[*unitInstance]bool, len(pending))
		for len(pending) > 0 {
			last := len(pending) - 1
			instance := pending[last].instance
			pending = pending[:last]
			if visited[instance] {
				continue
			}
			visited[instance] = true
			if !yield(instance) {
				return
			}
			pending = append(pending, instance.providerHolds...)
		}
	}
}

func (holds heldInstances) validate() error {
	for instance := range holds.closure() {
		if instance.lifetime.IsClosing() {
			return frame.ErrClosed
		}
	}
	return nil
}

func (i *unitInstance) hold() (instanceHold, error) {
	work, err := i.lifetime.Hold()
	if err != nil {
		return instanceHold{}, err
	}
	return instanceHold{instance: i, work: work}, nil
}

func (hold instanceHold) onInstanceCancel(cancel func(error)) func() bool {
	ctx := hold.instance.lifetime.Context()
	return context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
}

type preparationStatus uint8

const (
	preparationPending preparationStatus = iota
	preparationReady
	preparationFailed
)

// Readiness commits with catalog membership. Pending candidate reads end at
// that same conclusion; completed instances rely on their original holds.
// The channel only wakes waiters and cannot authorize serving.
type unitPreparation struct {
	status  preparationStatus
	failure error
	reads   []bindingRead
	done    chan struct{}
}

func newUnitPreparation(source UnitRef, reads []bindingRead) unitPreparation {
	preparation := unitPreparation{status: preparationPending, done: make(chan struct{})}
	for _, read := range reads {
		if read.unit == source {
			preparation.reads = append(preparation.reads, read)
		}
	}
	return preparation
}

func (p *unitPreparation) complete(failure error) {
	if p.status != preparationPending {
		panic("host: preparation already completed")
	}
	p.reads = nil
	p.failure = failure
	if failure == nil {
		p.status = preparationReady
	} else {
		p.status = preparationFailed
	}
	close(p.done)
}

// Preparation and retirement are independent: retirement can be requested
// while preparation runs and cannot rewrite its result. Host.mu guards mutation.
type unitInstance struct {
	host          *hostState
	id            string
	generation    uint64
	scope         ScopeGrant
	realm         RealmID
	unit          UnitDeclaration
	binding       UnitBinding
	snapshot      BindingSnapshot
	identity      framevalue.Digest
	configuration []byte
	setup         SetupFunc
	lifetime      frame.Lifetime
	resource      frame.ResourceHandle
	providerHolds heldInstances
	stops         []func() bool

	// startGrant is fixed before preparation; an empty grant is installation-scoped.
	// Only matching start claims may request retirement of a scoped instance.
	startGrant ScopeGrant
	starts     int

	exports *unitExports

	preparation unitPreparation
	retirement  instanceRetirement
}

const instanceResourcePrefix = "unit:"

func instanceResourceName(id string) string { return instanceResourcePrefix + id }

func (i *unitInstance) key() instanceKey {
	return instanceKey{source: i.binding.Source, realm: i.realm}
}

func (i *unitInstance) isCurrentLocked() bool {
	return i.host.instances[i.key()] == i
}

// Withdrawal commits with export removal in the original current membership;
// a later retirement or start claim cannot withdraw a successor.
func (i *unitInstance) withdrawLocked() {
	if !i.isCurrentLocked() {
		return
	}
	i.exports.withdraw()
	delete(i.host.instances, i.key())
}

// grant is the authority under which the setup runs: the starting grant of a
// started unit, otherwise its installation's grant.
func (i *unitInstance) grant() ScopeGrant {
	if i.startGrant.issued() {
		return i.startGrant
	}
	return i.scope
}

func (i *unitInstance) validateServingLocked() error {
	if i.preparation.status != preparationReady {
		return errors.Join(ErrUnavailable, i.preparation.failure)
	}
	return i.validateCurrentLocked()
}

func (i *unitInstance) observationLocked() InstanceObservation {
	failure := i.preparation.failure
	state := Preparing
	switch {
	case !i.isCurrentLocked():
		state = Draining
	case i.preparation.status == preparationReady:
		state = Ready
	}
	observation := InstanceObservation{ID: i.id, Generation: i.generation, Source: i.binding.Source, State: state, Failure: failure}
	if outcome, closing := i.lifetime.CloseOutcome(); closing {
		observation.Close = &outcome
		switch {
		case outcome.Released:
			observation.State = Closed
		case len(outcome.Failures) > 0:
			observation.State = CloseFailed
		default:
			observation.State = Closing
		}
	}
	return observation
}

// holdProvidersLocked holds every provider that the binding planned before i,
// so a provider cannot retire beneath its consumer.
func (i *unitInstance) holdProvidersLocked(planned map[UnitRef]*unitInstance) ([]frame.ResourceHandle, error) {
	var resources []frame.ResourceHandle
	held := make(map[*unitInstance]bool)
	for _, requirement := range i.binding.Imports {
		for _, source := range requirement.Providers {
			if source.Unit() == i.binding.Source {
				continue
			}
			provider := planned[source.Unit()]
			if provider == nil {
				return nil, fmt.Errorf("%w: provider not ordered before consumer", frame.ErrInvalid)
			}
			if held[provider] {
				continue
			}
			held[provider] = true
			hold, err := provider.hold()
			if err != nil {
				return nil, err
			}
			i.providerHolds = append(i.providerHolds, hold)
			resources = append(resources, provider.resource)
		}
	}
	return resources, nil
}

// Provider holds protect the frozen graph even after preparation fails. They
// end at the original resource exit, or when graph registration is refused.
func (i *unitInstance) releaseProvidersLocked() {
	for _, stop := range i.stops {
		stop()
	}
	for _, hold := range i.providerHolds {
		hold.work.Finish()
	}
	i.stops = nil
	i.providerHolds = nil
}

func (i *unitInstance) prepare() {
	err := errors.New("host: preparation exited without returning")
	// Codecs and driver validation share the original preparation's failure
	// boundary, including panic and Goexit, before anything becomes visible.
	defer func() {
		if recovered := recover(); recovered != nil {
			err = frame.CallbackPanicError("preparation", recovered)
		}
		if err != nil {
			i.failPreparation(err)
		}
	}()
	err = i.prepareUnit()
}

func (i *unitInstance) prepareUnit() error {
	h := i.host
	work, err := i.lifetime.Enter(i.lifetime.Context())
	if err != nil {
		return err
	}
	defer work.Finish()
	for _, hold := range i.providerHolds {
		provider := hold.instance
		select {
		case <-provider.preparation.done:
		case <-work.Context().Done():
			return frame.ContextError(work.Context())
		}
		h.mu.Lock()
		err := provider.validateServingLocked()
		h.mu.Unlock()
		if err != nil {
			return err
		}
	}
	setup := Setup{&setupCall{host: h, instance: i, work: work}}
	h.mu.Lock()
	run, err := i.setupFuncLocked()
	if err == nil {
		setup.imports, err = i.bindImportsLocked()
	}
	h.mu.Unlock()
	if err != nil {
		return err
	}
	if err := i.validateCurrent(work.Context()); err != nil {
		return err
	}
	exports, activate, err := invokeSetup(run, work.Context(), setup)
	if err == nil {
		err = frame.ContextError(work.Context())
	}
	if err != nil {
		return err
	}
	return i.publishPrepared(exports, activate)
}

// setupFuncLocked returns the native setup or the registered driver for the
// unit's profile. A driver never chooses another implementation.
func (i *unitInstance) setupFuncLocked() (SetupFunc, error) {
	if i.unit.Profile == NativeGo {
		return i.setup, nil
	}
	driver := i.host.drivers[i.unit.Profile].Driver
	if driver == nil {
		return nil, ErrUnavailable
	}
	return driver.Prepare, nil
}

func (i *unitInstance) bindImportsLocked() (map[string][]BoundValue, error) {
	imports := make(map[string][]BoundValue, len(i.binding.Imports))
	for _, requirement := range i.binding.Imports {
		values := make([]BoundValue, 0, len(requirement.Providers))
		for _, source := range requirement.Providers {
			if source.Unit() == i.binding.Source {
				values = append(values, BoundValue{Source: source, Local: true})
				continue
			}
			export, err := i.providerExportLocked(source)
			if err != nil {
				return nil, err
			}
			values = append(values, BoundValue{Source: source, Value: export.value})
		}
		imports[requirement.Slot] = values
	}
	return imports, nil
}

func (i *unitInstance) providerExportLocked(source ExportRef) (*publication, error) {
	for _, hold := range i.providerHolds {
		provider := hold.instance
		if provider.binding.Source == source.Unit() {
			return provider.exports.bound(source)
		}
	}
	return nil, frame.ErrContractMismatch
}

func (i *unitInstance) validateImportsLocked() error {
	for _, requirement := range i.binding.Imports {
		for _, source := range requirement.Providers {
			if source.Unit() == i.binding.Source {
				continue
			}
			if _, err := i.providerExportLocked(source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *unitInstance) validateCurrentLocked() error {
	h := i.host
	if h.lifetime.IsClosing() || !i.isCurrentLocked() || i.lifetime.IsClosing() {
		return ErrStaleBinding
	}
	if i.startGrant.issued() {
		if err := h.admission.ValidateScope(i.startGrant); err != nil {
			return err
		}
	}
	if err := h.admission.ValidateScope(i.scope); err != nil {
		return err
	}
	if err := i.providerHolds.validate(); err != nil {
		return errors.Join(ErrStaleBinding, err)
	}
	for provider := range i.providerHolds.closure() {
		if !provider.isCurrentLocked() {
			return ErrStaleBinding
		}
	}
	if err := h.validateBindingLocked(i.snapshot); err != nil {
		return err
	}
	if i.preparation.status == preparationPending {
		if err := i.validateImportsLocked(); err != nil {
			return err
		}
	}
	return h.validateBindingReadsLocked(i.preparation.reads)
}

func (i *unitInstance) validateCurrent(ctx context.Context) error {
	return i.host.admission.Under(ctx, func() error {
		i.host.mu.Lock()
		defer i.host.mu.Unlock()
		return i.validateCurrentLocked()
	})
}

func (i *unitInstance) publishPrepared(exports []ExportValue, activate func(context.Context) error) error {
	h := i.host
	publications, err := h.prepareSetupExports(i.lifetime.Context(), i, exports)
	if err != nil {
		return err
	}
	if activate != nil {
		err = i.validateCurrent(i.lifetime.Context())
		if err == nil {
			err = activate(i.lifetime.Context())
		}
		if err != nil {
			return err
		}
	}
	return h.admission.Admit(i.lifetime.Context(), func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		return i.publishSetupLocked(publications)
	})
}

func (i *unitInstance) failPreparation(err error) {
	h := i.host
	h.mu.Lock()
	i.preparation.complete(err)
	i.withdrawLocked()
	h.mu.Unlock()
	i.beginRetire()
}

func (i *unitInstance) publishSetupLocked(publications []*publication) error {
	if i.preparation.status != preparationPending {
		return ErrStaleBinding
	}
	if err := i.validateCurrentLocked(); err != nil {
		return err
	}
	for _, hold := range i.providerHolds {
		provider := hold.instance
		if !provider.isCurrentLocked() || provider.preparation.status != preparationReady {
			return ErrStaleBinding
		}
	}
	if err := i.exports.bindSetup(publications); err != nil {
		return err
	}
	i.preparation.complete(nil)
	return nil
}

// Cancellation ends permission immediately; retirement still joins the
// original preparation, consumers and resource-release evidence.
func (i *unitInstance) revoke(cause error) {
	i.lifetime.Cancel(cause)
	i.beginRetire()
}

func (i *unitInstance) beginRetire() {
	i.retirement.once.Do(func() {
		causes := i.retirement.causes
		go func() {
			h := i.host
			h.mu.Lock()
			i.withdrawLocked()
			pending := i.preparation.status == preparationPending
			h.mu.Unlock()
			if pending {
				i.lifetime.Cancel(frame.ErrClosed)
			}
			<-i.preparation.done
			if err := i.lifetime.AwaitUnheld(context.Background()); err != nil {
				panic(err)
			}
			exit := i.lifetime.BeginClose(frame.ErrClosed)
			i.retirement.publishExit(exit)
			outcome, _ := exit.WaitReleased(context.Background())
			h.mu.Lock()
			i.releaseProvidersLocked()
			h.mu.Unlock()
			_ = i.resource.ConfirmReleased(causes.Join(outcome.Err()))
			h.mu.Lock()
			delete(h.allInstances, i.id)
			i.exports = nil
			i.setup = nil
			i.configuration = nil
			i.retirement.causes = nil
			close(i.retirement.done)
			h.mu.Unlock()
		}()
	})
}

// registerInstanceLocked makes instance a Host resource that retires after its
// consumers, and retires it when a provider, its start grant or itself ends.
func (h *hostState) registerInstanceLocked(instance *unitInstance, providers []frame.ResourceHandle) error {
	causes := instance.retirement.causes
	resource, err := h.lifetime.Own(frame.Resource{
		Name:   instanceResourceName(instance.id),
		Cancel: instance.revoke,
		Close: func(ctx context.Context) (frame.ReleaseState, error) {
			instance.beginRetire()
			outcome, err := instance.retirement.awaitExit(ctx)
			return outcome.ResourceState(causes.Join(err))
		},
	}, providers...)
	if err != nil {
		return err
	}
	instance.resource = resource
	h.instances[instance.key()] = instance
	h.allInstances[instance.id] = instance
	for _, hold := range instance.providerHolds {
		instance.stops = append(instance.stops, hold.onInstanceCancel(instance.revoke))
	}
	if instance.startGrant.issued() {
		instance.stops = append(instance.stops, context.AfterFunc(instance.startGrant.context, func() { instance.revoke(ErrFenced) }))
	}
	instance.stops = append(instance.stops, context.AfterFunc(instance.lifetime.Context(), instance.beginRetire))
	return nil
}

// Retire withdraws every current instance of an installation. Instances keep
// their resources until their consumers have drained.
func (h *hostState) Retire(ctx context.Context, id InstallationID) error {
	var retired []*unitInstance
	err := h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		for key, instance := range h.instances {
			if key.source.InstallationID == id {
				retired = append(retired, instance)
			}
		}
		for _, instance := range retired {
			instance.withdrawLocked()
		}
		return nil
	})
	for _, instance := range retired {
		instance.beginRetire()
	}
	return err
}

func (h *hostState) RetireInstance(ctx context.Context, id string) error {
	var instance *unitInstance
	err := h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		instance = h.allInstances[id]
		if instance == nil {
			return ErrUnavailable
		}
		instance.withdrawLocked()
		return nil
	})
	if err != nil {
		return err
	}
	instance.beginRetire()
	return nil
}

// WaitInstanceRetired joins exactly the original instance and does not wait for
// other Units or later generations of the same installation.
func (h *hostState) WaitInstanceRetired(ctx context.Context, id string) error {
	if framevalue.IsNil(ctx) {
		return frame.ErrInvalid
	}
	h.mu.Lock()
	instance := h.allInstances[id]
	h.mu.Unlock()
	if instance == nil {
		return nil
	}
	return awaitRetired(ctx, instance.retirement.done)
}

func (h *hostState) WaitRetired(ctx context.Context, id InstallationID) error {
	if framevalue.IsNil(ctx) {
		return frame.ErrInvalid
	}
	h.mu.Lock()
	var instances []*unitInstance
	for _, instance := range h.allInstances {
		if instance.binding.Source.InstallationID == id {
			instances = append(instances, instance)
		}
	}
	h.mu.Unlock()
	for _, instance := range instances {
		if err := awaitRetired(ctx, instance.retirement.done); err != nil {
			return err
		}
	}
	return nil
}

// A confirmed retirement cannot be revoked by cancellation of its observer.
func awaitRetired(ctx context.Context, retired <-chan struct{}) error {
	select {
	case <-retired:
		return nil
	case <-ctx.Done():
		select {
		case <-retired:
			return nil
		default:
			return frame.ContextError(ctx)
		}
	}
}

func invokeSetup(setup SetupFunc, ctx context.Context, value Setup) (exports []ExportValue, activate func(context.Context) error, err error) {
	call := value.setupCall
	defer func() {
		exports, activate = call.seal(exports)
	}()
	exports, err = setup(ctx, value)
	return exports, activate, err
}
