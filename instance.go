package frame

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
	Close      *CloseOutcome
}

type instanceKey struct {
	source UnitRef
	realm  RealmID
}

// unitInstance is one actual setup of an exact unit binding. Preparation and
// retirement are independent facts: a retirement may be requested while the
// preparation is still running, and it never rewrites the preparation result.
// Host.mu guards every mutable field.
type unitInstance struct {
	host          *Host
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
	// preparation retains the candidate reads this setup depends on until
	// it is published; afterwards its exact providers are held directly.
	preparation []bindingRead
	lifetime    *Lifetime
	resource    *ResourceHandle
	providers   []*unitInstance
	// providerHolds keep each provider from retiring beneath this consumer.
	providerHolds []*Work
	stops         []func() bool

	// startGrant is fixed before preparation; an empty grant is installation-scoped.
	// Only matching start claims may request retirement of a scoped instance.
	startGrant ScopeGrant
	starts     int

	setupExports          map[string]*publication
	publicationOperations map[string]*publicationOperation

	prepared       chan struct{}
	preparationErr error
	retiring       bool
	retireOnce     sync.Once
	closed         chan struct{}
}

type publicationOperation struct {
	request     publicationRequestID
	publication *publication
}

const instanceResourcePrefix = "unit:"

func instanceResourceName(id string) string { return instanceResourcePrefix + id }

func (i *unitInstance) key() instanceKey {
	return instanceKey{source: i.binding.Source, realm: i.realm}
}

// grant is the authority under which the setup runs: the starting grant of a
// started unit, otherwise its installation's grant.
func (i *unitInstance) grant() ScopeGrant {
	if i.startGrant.issued() {
		return i.startGrant
	}
	return i.scope
}

func (i *unitInstance) preparedLocked() (bool, error) {
	select {
	case <-i.prepared:
		return true, i.preparationErr
	default:
		return false, nil
	}
}

func (i *unitInstance) validateServingLocked() error {
	done, err := i.preparedLocked()
	if !done || err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	return i.validateCurrentLocked()
}

func (i *unitInstance) observationLocked() InstanceObservation {
	done, err := i.preparedLocked()
	state := Preparing
	switch {
	case i.retiring:
		state = Draining
	case done:
		state = Ready
	}
	observation := InstanceObservation{ID: i.id, Generation: i.generation, Source: i.binding.Source, State: state, Failure: err}
	if outcome, closing := i.lifetime.closeOutcome(); closing {
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
func (i *unitInstance) holdProvidersLocked(planned map[UnitRef]*unitInstance) ([]*ResourceHandle, error) {
	var resources []*ResourceHandle
	held := make(map[*unitInstance]bool)
	for _, requirement := range i.binding.Imports {
		for _, source := range requirement.Providers {
			if source.Unit() == i.binding.Source {
				continue
			}
			provider := planned[source.Unit()]
			if provider == nil {
				return nil, fmt.Errorf("%w: provider not ordered before consumer", ErrInvalid)
			}
			if held[provider] {
				continue
			}
			held[provider] = true
			hold, err := provider.lifetime.hold()
			if err != nil {
				return nil, err
			}
			i.providers = append(i.providers, provider)
			i.providerHolds = append(i.providerHolds, hold)
			resources = append(resources, provider.resource)
		}
	}
	return resources, nil
}

// releaseProvidersLocked undoes holdProvidersLocked for an instance that never
// became a Host resource, so a partial graph cannot pin its providers.
func (i *unitInstance) releaseProvidersLocked() {
	for _, stop := range i.stops {
		stop()
	}
	for _, hold := range i.providerHolds {
		hold.Finish()
	}
	i.stops = nil
	i.providerHolds = nil
	i.providers = nil
}

func (i *unitInstance) prepare() {
	err := errors.New("frame: preparation exited without returning")
	// Codecs and driver validation share the original preparation's failure
	// boundary, including panic and Goexit, before anything becomes visible.
	defer func() { i.finishPrepare(err) }()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = callbackPanicError("preparation", recovered)
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
	for _, provider := range i.providers {
		select {
		case <-provider.prepared:
		case <-work.Context().Done():
			return contextError(work.Context())
		}
		h.mu.Lock()
		err := provider.validateServingLocked()
		h.mu.Unlock()
		if err != nil {
			return err
		}
	}
	setup := &Setup{host: h, instance: i, work: work, activation: &activation{}}
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
		err = contextError(work.Context())
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
	for _, provider := range i.providers {
		if provider.binding.Source == source.Unit() {
			return provider.setupExportLocked(source)
		}
	}
	return nil, ErrContractMismatch
}

// A declaration names the original setup export. Live Point membership can
// withdraw that value, but cannot replace what a new setup would receive.
func (i *unitInstance) setupExportLocked(source ExportRef) (*publication, error) {
	export := i.setupExports[source.Export]
	if export == nil || export.source != source {
		return nil, ErrContractMismatch
	}
	if !export.visible || !export.durable {
		return nil, ErrUnavailable
	}
	return export, nil
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
	if h.lifetime.isClosing() || i.retiring || h.instances[i.key()] != i || i.lifetime.isClosing() {
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
	if err := h.validateBindingLocked(i.snapshot); err != nil {
		return err
	}
	if done, _ := i.preparedLocked(); !done {
		if err := i.validateImportsLocked(); err != nil {
			return err
		}
	}
	return h.validateBindingReadsLocked(i.preparation)
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
	publications, err := h.setupPublications(i.lifetime.Context(), i, exports)
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

func (i *unitInstance) finishPrepare(err error) {
	h := i.host
	h.mu.Lock()
	i.preparation = nil
	i.preparationErr = err
	close(i.prepared)
	if err != nil {
		h.retireInstanceLocked(i)
	}
	h.mu.Unlock()
	if err != nil {
		i.beginRetire()
	}
}

func (i *unitInstance) publishSetupLocked(publications []*publication) error {
	if err := i.validateCurrentLocked(); err != nil {
		return err
	}
	for _, provider := range i.providers {
		if provider.retiring || provider.preparationErr != nil {
			return ErrStaleBinding
		}
	}
	if err := i.host.catalog.reserve(len(publications)); err != nil {
		return err
	}
	for _, export := range publications {
		i.host.catalog.publish(export)
		i.setupExports[export.source.Export] = export
	}
	return nil
}

func (i *unitInstance) withdrawLocked() {
	for _, export := range i.setupExports {
		i.host.catalog.withdraw(export)
	}
	for _, operation := range i.publicationOperations {
		i.host.catalog.withdraw(operation.publication)
	}
}

// retire drains the consumers holding i before closing its resources. A
// preparation still running is cancelled; consumers are never cancelled here.
func (i *unitInstance) retire(ctx context.Context) (CloseOutcome, error) {
	h := i.host
	h.mu.Lock()
	h.retireInstanceLocked(i)
	done, _ := i.preparedLocked()
	h.mu.Unlock()
	if !done {
		i.lifetime.cancel(ErrClosed)
	}
	select {
	case <-i.prepared:
	case <-ctx.Done():
		return CloseOutcome{}, contextError(ctx)
	}
	if err := i.lifetime.awaitUnheld(ctx); err != nil {
		return CloseOutcome{}, err
	}
	return i.lifetime.Close(ctx)
}

func (i *unitInstance) beginRetire() {
	i.retireOnce.Do(func() {
		causes := i.lifetime.ObserveCloseCauses()
		go func() {
			_, _ = i.retire(context.Background())
			outcome, _ := i.lifetime.BeginClose(ErrClosed).WaitReleased(context.Background())
			if !outcome.Released {
				return
			}
			h := i.host
			h.mu.Lock()
			i.releaseProvidersLocked()
			h.mu.Unlock()
			_ = i.resource.ConfirmReleased(causes.join(outcomeError(outcome)))
			h.mu.Lock()
			if h.instances[i.key()] == i {
				delete(h.instances, i.key())
			}
			delete(h.allInstances, i.id)
			i.setupExports = nil
			i.setup = nil
			i.configuration = nil
			i.publicationOperations = nil
			close(i.closed)
			h.mu.Unlock()
		}()
	})
}

func invokeSetup(setup SetupFunc, ctx context.Context, value *Setup) (exports []ExportValue, activate func(context.Context) error, err error) {
	defer func() {
		activate = value.sealPreparation()
	}()
	exports, err = setup(ctx, value)
	return exports, activate, err
}
