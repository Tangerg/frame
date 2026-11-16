package host

import (
	"context"
	"errors"

	"github.com/Tangerg/frame"
)

type preparationStatus uint8

const (
	preparationPending preparationStatus = iota
	preparationReady
	preparationFailed
)

// Readiness commits with catalog membership. Pending candidate reads end at
// that same conclusion; completed instances rely on their original holds.
// Reserving the original interval before start claims protects the capacity
// needed to execute preparation. Its conclusion also ends an abandoned plan.
// The channel only wakes waiters and cannot authorize serving.
type unitPreparation struct {
	status  preparationStatus
	failure error
	reads   []bindingRead
	work    frame.Work
	done    chan struct{}
}

func newUnitPreparation(lifetime frame.Lifetime, source UnitRef, reads []bindingRead) (unitPreparation, error) {
	work, err := lifetime.Enter(lifetime.Context())
	if err != nil {
		return unitPreparation{}, err
	}
	preparation := unitPreparation{status: preparationPending, work: work, done: make(chan struct{})}
	for _, read := range reads {
		if read.unit == source {
			preparation.reads = append(preparation.reads, read)
		}
	}
	return preparation, nil
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
	p.work.Finish()
	p.work = frame.Work{}
	close(p.done)
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
	work := i.preparation.work
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
	i.beginRetireLocked()
	h.mu.Unlock()
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

func invokeSetup(setup SetupFunc, ctx context.Context, value Setup) (exports []ExportValue, activate func(context.Context) error, err error) {
	call := value.setupCall
	defer func() {
		exports, activate = call.seal(exports)
	}()
	exports, err = setup(ctx, value)
	return exports, activate, err
}
