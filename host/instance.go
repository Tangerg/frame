package host

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

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
	startGrant  ScopeGrant
	startClaims map[*startClaim]struct{}

	exports *unitExports

	preparation unitPreparation

	// Only this worker drains preparation and consumers. The barrier admits
	// observers to the Lifetime's close without another close owner.
	closeReady  chan struct{}
	retired     chan struct{}
	closeCauses *frame.CloseCauses
}

const instanceResourcePrefix = "unit:"

func instanceResourceName(id string) string { return instanceResourcePrefix + id }

func (i *unitInstance) key() instanceKey {
	return instanceKey{source: i.binding.Source, realm: i.realm}
}

func (i *unitInstance) isCurrentLocked() bool {
	return i.host.instances[i.key()] == i
}

// grant is the authority under which the setup runs: the starting grant of a
// started unit, otherwise its installation's grant.
func (i *unitInstance) grant() ScopeGrant {
	if i.startGrant.Valid() {
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

// Portable calls may cross Realms, but a binding has no per-provider Realm
// override. Recovery must realize the same affinity throughout the held graph.
func (i *unitInstance) realizesRealm(realm RealmID) bool {
	if i.realm != "" && i.realm != realm {
		return false
	}
	for provider := range i.providerHolds.closure() {
		if provider.realm != "" && provider.realm != realm {
			return false
		}
	}
	return true
}

// Binding identity names declarations, not physical executions. A shared
// consumer cannot retarget its original holds to another Realm's instances.
func (i *unitInstance) validateReuseLocked(identity framevalue.Digest, providers map[UnitRef]*unitInstance, grant ScopeGrant) error {
	if i.startGrant.Valid() && i.startGrant != grant {
		return ErrFenced
	}
	if i.identity != identity {
		return fmt.Errorf("%w: unit already has another setup binding", ErrInUse)
	}
	for _, hold := range i.providerHolds {
		if providers[hold.instance.binding.Source] != hold.instance {
			return fmt.Errorf("%w: unit retains another provider instance", ErrInUse)
		}
	}
	return nil
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
			if source.UnitRef == i.binding.Source {
				continue
			}
			provider := planned[source.UnitRef]
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

func (i *unitInstance) validateCurrentLocked() error {
	h := i.host
	if h.lifetime.IsClosing() || !i.isCurrentLocked() || i.lifetime.IsClosing() {
		return ErrStaleBinding
	}
	if i.startGrant.Valid() {
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

// Cancellation ends permission immediately; retirement still joins the
// original preparation, consumers and resource-release evidence.
func (i *unitInstance) revoke(cause error) {
	i.lifetime.Cancel(cause)
	i.beginRetire()
}

func (i *unitInstance) beginRetire() {
	h := i.host
	h.mu.Lock()
	defer h.mu.Unlock()
	i.beginRetireLocked()
}

// Removing current membership starts its one retirement worker. A caller cannot
// leave it withdrawn without a continuation, or withdraw a successor.
func (i *unitInstance) beginRetireLocked() {
	if !i.isCurrentLocked() {
		return
	}
	i.exports.withdraw()
	delete(i.host.instances, i.key())
	causes := i.closeCauses
	go func() {
		h := i.host
		i.endStartClaims()
		h.mu.Lock()
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
		close(i.closeReady)
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
		i.closeCauses = nil
		i.startClaims = nil
		close(i.retired)
		h.mu.Unlock()
	}()
}

func (i *unitInstance) awaitExit(ctx context.Context) (frame.CloseOutcome, error) {
	if err := awaitRetired(ctx, i.closeReady); err != nil {
		return frame.CloseOutcome{}, err
	}
	return i.lifetime.Close(ctx)
}

// registerInstanceLocked makes instance a Host resource that retires after its
// consumers, and retires it when a provider, its start grant or itself ends.
func (h *hostState) registerInstanceLocked(instance *unitInstance, providers []frame.ResourceHandle) error {
	causes := instance.closeCauses
	resource, err := h.lifetime.Own(frame.Resource{
		Name:   instanceResourceName(instance.id),
		Cancel: instance.revoke,
		Close: func(ctx context.Context) (frame.ReleaseState, error) {
			instance.beginRetire()
			outcome, err := instance.awaitExit(ctx)
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
		instance.stops = append(instance.stops, hold.onInstanceCancel(func(cause error) {
			instance.revoke(errors.Join(ErrStaleBinding, cause))
		}))
	}
	if instance.startGrant.Valid() {
		instance.stops = append(instance.stops, context.AfterFunc(instance.startGrant.context, func() { instance.revoke(ErrFenced) }))
	}
	instance.stops = append(instance.stops, context.AfterFunc(instance.lifetime.Context(), instance.beginRetire))
	return nil
}

// Retire withdraws every current instance of an installation. Instances keep
// their resources until their consumers have drained.
func (h *hostState) Retire(ctx context.Context, id InstallationID) error {
	return h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		for key, instance := range h.instances {
			if key.source.InstallationID == id {
				instance.beginRetireLocked()
			}
		}
		return nil
	})
}

func (h *hostState) RetireInstance(ctx context.Context, id string) error {
	return h.admission.Maintenance(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		instance := h.allInstances[id]
		if instance == nil {
			return ErrUnavailable
		}
		instance.beginRetireLocked()
		return nil
	})
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
	return awaitRetired(ctx, instance.retired)
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
		if err := awaitRetired(ctx, instance.retired); err != nil {
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
