package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// AcceptedBindingReader reads a currently authorized business record under
// Host admission, before preparation and again before the final grant. Reading
// must not consume the record or change its authorization.
type AcceptedBindingReader func(ctx context.Context, recordID string, scope ScopeGrant) (BindingSnapshot, error)

type Config struct {
	ReadAcceptedBinding AcceptedBindingReader
	Admission           Admission
	Lifetime            frame.Lifetime
	Epoch               uint64
	Drivers             []Driver
	UseOwners           []UseOwner
	Installations       InstallationStore
	ExecutionProtection ExecutionProtection
	Limits              Limits
}

// HostLimits bound every record a Host retains on behalf of its callers. A
// zero limit selects its default.
type Limits struct {
	MaxInstallations         int
	MaxPublications          int
	MaxPublicationOperations int
	MaxWatches               int
	WatchBuffer              int
	MaxUses                  int
	MaxEventRoutes           int
}

const (
	defaultMaxInstallations         = 4096
	defaultMaxPublications          = 4096
	defaultMaxPublicationOperations = 8192
	defaultMaxWatches               = 1024
	defaultWatchBuffer              = 64
	defaultMaxUses                  = 4096
	defaultMaxEventRoutes           = 4096
)

func (l Limits) resolve() (Limits, error) {
	for _, limit := range []struct {
		name     string
		value    *int
		fallback int
	}{
		{"installations", &l.MaxInstallations, defaultMaxInstallations},
		{"publications", &l.MaxPublications, defaultMaxPublications},
		{"publication operations", &l.MaxPublicationOperations, defaultMaxPublicationOperations},
		{"watches", &l.MaxWatches, defaultMaxWatches},
		{"watch buffer", &l.WatchBuffer, defaultWatchBuffer},
		{"uses", &l.MaxUses, defaultMaxUses},
		{"event routes", &l.MaxEventRoutes, defaultMaxEventRoutes},
	} {
		if err := frame.ResolveLimit(limit.name, limit.value, limit.fallback); err != nil {
			return Limits{}, err
		}
	}
	return l, nil
}

// Host owns installations, admitted bindings, actual instances and Uses. Its
// mutex guards every field below it and is never held while plugin, driver or
// storage code runs.
type Host struct{ *hostState }

type hostState struct {
	identity            *frame.LifetimeDomain
	admission           Admission
	lifetime            frame.Lifetime
	epoch               uint64
	limits              Limits
	store               InstallationStore
	protection          ExecutionProtection
	readAcceptedBinding AcceptedBindingReader

	mu                     sync.Mutex
	drivers                map[Profile]registeredDriver
	contracts              contractRegistry
	installations          map[InstallationID]*installed
	installationTransition *installationTransition
	// instances indexes the current instance per unit and realm; allInstances
	// retains every instance until its retirement has been confirmed.
	instances      map[instanceKey]*unitInstance
	allInstances   map[string]*unitInstance
	nextGeneration uint64
	catalog        catalog
	uses           map[string]*useBorrow
	routes         map[string]*eventRoute
}

func New(ctx context.Context, config Config) (_ Host, err error) {
	if err := frame.ContextError(ctx); err != nil {
		return Host{}, err
	}
	if config.Installations != nil && framevalue.IsNil(config.Installations) || config.ExecutionProtection != nil && framevalue.IsNil(config.ExecutionProtection) {
		return Host{}, frame.ErrInvalid
	}
	limits, err := config.Limits.resolve()
	if err != nil {
		return Host{}, err
	}
	if !config.Admission.Valid() {
		config.Admission = NewAdmission()
	}
	if config.Epoch == 0 {
		config.Epoch = 1
	}
	if !config.Lifetime.Valid() {
		config.Lifetime = frame.NewLifetime(ctx)
		defer func() {
			if err != nil {
				config.Lifetime.BeginClose(err)
			}
		}()
	}
	if config.Lifetime.HasDomain() {
		return Host{}, fmt.Errorf("%w: host lifetime belongs to another authority", frame.ErrInvalid)
	}
	if config.Lifetime.IsClosing() {
		return Host{}, frame.ErrClosed
	}
	h := &hostState{
		identity:            new(frame.LifetimeDomain),
		admission:           config.Admission,
		lifetime:            config.Lifetime,
		epoch:               config.Epoch,
		limits:              limits,
		store:               config.Installations,
		protection:          config.ExecutionProtection,
		readAcceptedBinding: config.ReadAcceptedBinding,
		drivers:             make(map[Profile]registeredDriver),
		contracts:           newContractRegistry(),
		installations:       make(map[InstallationID]*installed),
		instances:           make(map[instanceKey]*unitInstance),
		allInstances:        make(map[string]*unitInstance),
		catalog:             newCatalog(limits.MaxPublications),
		uses:                make(map[string]*useBorrow),
		routes:              make(map[string]*eventRoute),
	}
	if err := h.validateDurableAuthority(); err != nil {
		return Host{}, err
	}
	for _, driver := range config.Drivers {
		if err := h.RegisterDriver(driver); err != nil {
			return Host{}, err
		}
	}
	if err := h.admission.registerUseOwners(config.UseOwners); err != nil {
		return Host{}, err
	}
	return Host{h}, nil
}

func (h *hostState) newLifetime(ctx context.Context) frame.Lifetime {
	return h.identity.NewLifetime(ctx)
}

func (h *hostState) Admission() Admission { return h.admission }
func (h *hostState) Epoch() uint64        { return h.epoch }

func (h *hostState) RegisterDriver(driver Driver) error {
	if framevalue.IsNil(driver) {
		return frame.ErrInvalid
	}
	profile := driver.Profile()
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile == NativeGo {
		return fmt.Errorf("%w: native setup belongs to its declaration", frame.ErrInvalid)
	}
	registered := registeredDriver{Driver: driver}
	if bound, ok := driver.(RealmDriver); ok {
		registered.realmBound = bound.RealmBound()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.IsClosing() {
		return frame.ErrClosed
	}
	if _, exists := h.drivers[profile]; exists {
		return fmt.Errorf("%w: duplicate execution profile", frame.ErrInvalid)
	}
	h.drivers[profile] = registered
	return nil
}

func (h *hostState) Driver(profile Profile) (Driver, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	driver, ok := h.drivers[profile]
	return driver.Driver, ok
}

// realmLocked addresses a realm only for drivers that create one instance per
// realm; every other unit has a single realm-independent instance.
func (h *hostState) realmLocked(unit UnitDeclaration, realm RealmID) (RealmID, error) {
	if !h.drivers[unit.Profile].realmBound {
		return "", nil
	}
	if err := realm.Validate(); err != nil {
		return "", err
	}
	return realm, nil
}

func (h *hostState) Observations() []InstanceObservation {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]InstanceObservation, 0, len(h.allInstances))
	for _, instance := range h.allInstances {
		result = append(result, instance.observationLocked())
	}
	slices.SortFunc(result, func(a, b InstanceObservation) int {
		return cmp.Or(compareUnit(a.Source, b.Source), cmp.Compare(a.Generation, b.Generation))
	})
	return result
}

func (h *hostState) Close(ctx context.Context) (frame.CloseOutcome, error) {
	if h == nil || framevalue.IsNil(ctx) {
		return frame.CloseOutcome{}, frame.ErrInvalid
	}
	type releaseRetry struct {
		settlement *settlement
		observed   *settlementFailure
	}
	var retries []releaseRetry
	causes := h.lifetime.ObserveCloseCauses()
	h.mu.Lock()
	for _, use := range h.uses {
		if observed, pending := use.settlement.requestRetry(); pending {
			retries = append(retries, releaseRetry{settlement: use.settlement, observed: observed})
		}
	}
	h.mu.Unlock()
	operation := h.lifetime.BeginClose(frame.ErrClosed)
	h.mu.Lock()
	for _, instance := range h.allInstances {
		instance.withdrawLocked()
	}
	for watch := range h.catalog.watches {
		watch.closeLocked(frame.ErrClosed)
	}
	h.mu.Unlock()
	var releaseErr error
	for _, retry := range retries {
		releaseErr = errors.Join(releaseErr, retry.settlement.await(ctx, retry.observed))
	}
	outcome, err := operation.Wait(ctx)
	return outcome, causes.Join(errors.Join(err, releaseErr))
}

func (h Host) Valid() bool { return h.hostState != nil }
