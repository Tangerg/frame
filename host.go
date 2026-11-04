package frame

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/Tangerg/frame/internal/nilness"
)

type UseApproval struct {
	ID              string
	ParentID        string
	ExecutionID     string
	Instances       []string
	Realm           RealmID
	Scope           ScopeID
	ScopeGeneration uint64
	Epoch           uint64
	Binding         BindingSnapshot
}

// ExecutionProtection persists an approved range before a surviving execution
// domain receives it. Release observes the same original use, never a new one.
type ExecutionProtection interface {
	ApproveUse(context.Context, UseApproval) error
	ReleaseUse(context.Context, string, ReleaseProof) error
}

// AcceptedBindingReader reads a currently authorized business record under
// Host admission, before preparation and again before the final grant. Reading
// must not consume the record or change its authorization.
type AcceptedBindingReader func(ctx context.Context, recordID string, scope ScopeGrant) (BindingSnapshot, error)

type HostConfig struct {
	ReadAcceptedBinding AcceptedBindingReader
	Admission           *Admission
	Lifetime            *Lifetime
	Epoch               uint64
	Drivers             []Driver
	UseOwners           []UseOwner
	Installations       InstallationStore
	ExecutionProtection ExecutionProtection
	Limits              HostLimits
}

// HostLimits bound every record a Host retains on behalf of its callers. A
// zero limit selects its default.
type HostLimits struct {
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

func (l HostLimits) resolve() (HostLimits, error) {
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
		if err := resolveLimit(limit.name, limit.value, limit.fallback); err != nil {
			return HostLimits{}, err
		}
	}
	return l, nil
}

// authorityIdentity marks values created by one Host. Its address, not its
// content, is the identity, so it cannot be forged by another Host.
type authorityIdentity struct{ _ byte }

// Host owns installations, admitted bindings, actual instances and Uses. Its
// mutex guards every field below it and is never held while plugin, driver or
// storage code runs.
type Host struct {
	identity            *authorityIdentity
	admission           *Admission
	lifetime            *Lifetime
	epoch               uint64
	limits              HostLimits
	store               InstallationStore
	protection          ExecutionProtection
	readAcceptedBinding AcceptedBindingReader

	mu            sync.Mutex
	drivers       map[Profile]registeredDriver
	contracts     contractRegistry
	installations map[InstallationID]*installed
	// instances indexes the current instance per unit and realm; allInstances
	// retains every instance until its retirement has been confirmed.
	instances      map[instanceKey]*Instance
	allInstances   map[string]*Instance
	nextGeneration uint64
	catalog        catalog
	uses           map[string]*Use
	routes         map[string]*eventRoute
}

func NewHost(ctx context.Context, config HostConfig) (_ *Host, err error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if config.Installations != nil && nilness.Is(config.Installations) || config.ExecutionProtection != nil && nilness.Is(config.ExecutionProtection) {
		return nil, ErrInvalid
	}
	limits, err := config.Limits.resolve()
	if err != nil {
		return nil, err
	}
	if config.Admission == nil {
		config.Admission = NewAdmission()
	}
	if config.Epoch == 0 {
		config.Epoch = 1
	}
	if config.Lifetime == nil {
		config.Lifetime = NewLifetime(ctx)
		defer func() {
			if err != nil {
				config.Lifetime.BeginClose(err)
			}
		}()
	}
	if config.Lifetime.isClosing() {
		return nil, ErrClosed
	}
	h := &Host{
		identity:            &authorityIdentity{},
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
		instances:           make(map[instanceKey]*Instance),
		allInstances:        make(map[string]*Instance),
		catalog:             newCatalog(limits.MaxPublications),
		uses:                make(map[string]*Use),
		routes:              make(map[string]*eventRoute),
	}
	for _, driver := range config.Drivers {
		if err := h.RegisterDriver(driver); err != nil {
			return nil, err
		}
	}
	if err := h.admission.registerUseOwners(config.UseOwners); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Host) Admission() *Admission { return h.admission }
func (h *Host) Epoch() uint64         { return h.epoch }

func (h *Host) RegisterDriver(driver Driver) error {
	if nilness.Is(driver) {
		return ErrInvalid
	}
	profile := driver.Profile()
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile == NativeGo {
		return fmt.Errorf("%w: native setup belongs to its declaration", ErrInvalid)
	}
	registered := registeredDriver{Driver: driver}
	if bound, ok := driver.(RealmDriver); ok {
		registered.realmBound = bound.RealmBound()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() {
		return ErrClosed
	}
	if _, exists := h.drivers[profile]; exists {
		return fmt.Errorf("%w: duplicate execution profile", ErrInvalid)
	}
	h.drivers[profile] = registered
	return nil
}

func (h *Host) Driver(profile Profile) (Driver, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	driver, ok := h.drivers[profile]
	return driver.Driver, ok
}

// realmLocked addresses a realm only for drivers that create one instance per
// realm; every other unit has a single realm-independent instance.
func (h *Host) realmLocked(unit UnitDeclaration, realm RealmID) (RealmID, error) {
	if !h.drivers[unit.Profile].realmBound {
		return "", nil
	}
	if err := realm.Validate(); err != nil {
		return "", err
	}
	return realm, nil
}

// retireInstanceLocked is the only transition that withdraws an instance from
// new work. The caller starts its retirement after releasing Host.mu.
func (h *Host) retireInstanceLocked(i *Instance) {
	i.retiring = true
	i.withdrawLocked()
	if h.instances[i.key()] == i {
		delete(h.instances, i.key())
	}
}

func (h *Host) Observations() []InstanceObservation {
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

func (h *Host) Close(ctx context.Context) (CloseOutcome, error) {
	causes := h.lifetime.ObserveCloseCauses()
	operation := h.lifetime.BeginClose(ErrClosed)
	h.mu.Lock()
	for _, instance := range h.allInstances {
		h.retireInstanceLocked(instance)
	}
	for watch := range h.catalog.watches {
		watch.closeLocked(ErrClosed)
	}
	h.mu.Unlock()
	outcome, err := operation.Wait(ctx)
	return outcome, causes.join(err)
}
