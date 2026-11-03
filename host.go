package frame

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"

	framevalue "github.com/Tangerg/frame/value"
)

type CommitState string

const (
	CommitSucceeded     CommitState = "committed"
	CommitRejected      CommitState = "notCommitted"
	CommitIndeterminate CommitState = "indeterminate"
)

type InstallationRecord struct {
	ID            InstallationID    `json:"id"`
	Release       framevalue.Digest `json:"release"`
	Configuration []byte            `json:"configuration"`
	Enabled       bool              `json:"enabled"`
	Units         []UnitDeclaration `json:"units"`
}

func (r InstallationRecord) Clone() InstallationRecord {
	r.Configuration = bytes.Clone(r.Configuration)
	r.Units = slices.Clone(r.Units)
	for i := range r.Units {
		r.Units[i] = r.Units[i].Clone()
	}
	return r
}

type InstallationChange struct {
	Before *InstallationRecord
	After  *InstallationRecord
}

// InstallationStore only encodes a transition already admitted by Host. A
// domain-specific use owner retains its own business rows and binding snapshots.
type InstallationStore interface {
	CommitInstallation(context.Context, InstallationChange) (CommitState, error)
	ConfirmInstallation(context.Context, InstallationID) error
	Installations(context.Context) ([]InstallationRecord, error)
}

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
	ReleaseUse(context.Context, string, CloseOutcome) error
}

// AcceptedBindingReader reads a currently authorized business record under
// Host admission, before preparation and again before the final grant. Reading
// must not consume the record or change its authorization.
type AcceptedBindingReader func(ctx context.Context, recordID string, scope ScopeGrant) (BindingSnapshot, error)

type HostConfig struct {
	ReadAcceptedBinding      AcceptedBindingReader
	Admission                *Admission
	Lifetime                 *Lifetime
	Epoch                    uint64
	Drivers                  []Driver
	UseOwners                []UseOwner
	Installations            InstallationStore
	ExecutionProtection      ExecutionProtection
	MaxInstallations         int
	MaxPublications          int
	MaxPublicationOperations int
	WatchCapacity            int
	MaxWatches               int
	MaxUses                  int
	MaxEventRoutes           int
}

const (
	defaultMaxInstallations         = 4096
	defaultMaxPublications          = 4096
	defaultMaxPublicationOperations = 8192
	defaultWatchCapacity            = 64
	defaultMaxWatches               = 1024
	defaultMaxUses                  = 4096
	defaultMaxEventRoutes           = 4096
)

type InstallationConfig struct {
	ID            InstallationID
	Plugin        *Plugin
	Configuration []byte
	Enabled       bool
}

type installed struct {
	record InstallationRecord
	plugin *Plugin
	scope  ScopeGrant
}

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

type Instance struct {
	activation            func(context.Context) error
	preparationSealed     bool
	startGrant            ScopeGrant
	starts                int
	host                  *Host
	id                    string
	generation            uint64
	scope                 ScopeGrant
	realm                 RealmID
	unit                  UnitDeclaration
	binding               UnitBinding
	snapshot              BindingSnapshot
	preparation           []bindingRead
	identity              framevalue.Digest
	configuration         []byte
	setup                 SetupFunc
	lifetime              *Lifetime
	hostResource          *ResourceHandle
	dependencies          []*Instance
	dependencyHolds       []*Work
	setupExports          map[string]*publication
	ready                 chan struct{}
	readyErr              error
	readyDone             bool
	retiring              bool
	retireOnce            sync.Once
	closed                chan struct{}
	uses                  int
	dependents            int
	stopFailure           func() bool
	stopDependencies      []func() bool
	changed               chan struct{}
	publicationOperations map[string]*publicationOperation
}

type publicationOperation struct {
	request     publicationRequestID
	publication *publication
}

type Host struct {
	readAcceptedBinding      AcceptedBindingReader
	mu                       sync.Mutex
	admission                *Admission
	lifetime                 *Lifetime
	epoch                    uint64
	identity                 *authorityIdentity
	drivers                  map[Profile]registeredDriver
	contracts                map[ContractRef]*declaration
	contractNames            map[nominalContract]ContractRef
	installations            map[InstallationID]*installed
	instances                map[instanceKey]*Instance
	allInstances             map[string]*Instance
	catalog                  map[ContractRef]*pointStore
	uses                     map[string]*Use
	watches                  map[*Watch]struct{}
	routes                   map[string]*eventRoute
	nextGeneration           uint64
	store                    InstallationStore
	protection               ExecutionProtection
	maxInstallations         int
	maxPublications          int
	maxPublicationOperations int
	watchCapacity            int
	maxWatches               int
	maxUses                  int
	maxEventRoutes           int
}

func NewHost(ctx context.Context, config HostConfig) (_ *Host, err error) {
	if ctx == nil || config.Installations != nil && nilValue(config.Installations) || config.ExecutionProtection != nil && nilValue(config.ExecutionProtection) {
		return nil, ErrInvalid
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if config.Admission == nil {
		config.Admission = NewAdmission()
	}
	if config.Epoch == 0 {
		config.Epoch = 1
	}
	if config.MaxInstallations == 0 {
		config.MaxInstallations = defaultMaxInstallations
	}
	if config.MaxPublications == 0 {
		config.MaxPublications = defaultMaxPublications
	}
	if config.MaxPublicationOperations == 0 {
		config.MaxPublicationOperations = defaultMaxPublicationOperations
	}
	if config.WatchCapacity == 0 {
		config.WatchCapacity = defaultWatchCapacity
	}
	if config.MaxWatches == 0 {
		config.MaxWatches = defaultMaxWatches
	}
	if config.MaxUses == 0 {
		config.MaxUses = defaultMaxUses
	}
	if config.MaxEventRoutes == 0 {
		config.MaxEventRoutes = defaultMaxEventRoutes
	}
	if config.MaxInstallations < 1 || config.MaxPublications < 1 || config.MaxPublicationOperations < 1 || config.WatchCapacity < 1 || config.MaxWatches < 1 || config.MaxUses < 1 || config.MaxEventRoutes < 1 {
		return nil, ErrInvalid
	}
	if config.Lifetime == nil {
		config.Lifetime = NewLifetime(ctx)
		defer func() {
			if err != nil {
				config.Lifetime.BeginClose(err)
			}
		}()
	}
	h := &Host{readAcceptedBinding: config.ReadAcceptedBinding, admission: config.Admission, lifetime: config.Lifetime, epoch: config.Epoch, identity: &authorityIdentity{}, drivers: make(map[Profile]registeredDriver), contracts: make(map[ContractRef]*declaration), contractNames: make(map[nominalContract]ContractRef), installations: make(map[InstallationID]*installed), instances: make(map[instanceKey]*Instance), allInstances: make(map[string]*Instance), catalog: make(map[ContractRef]*pointStore), uses: make(map[string]*Use), watches: make(map[*Watch]struct{}), store: config.Installations, protection: config.ExecutionProtection, maxPublications: config.MaxPublications, maxPublicationOperations: config.MaxPublicationOperations, watchCapacity: config.WatchCapacity}
	h.routes = make(map[string]*eventRoute)
	h.maxEventRoutes = config.MaxEventRoutes
	h.maxInstallations = config.MaxInstallations
	h.maxWatches = config.MaxWatches
	h.maxUses = config.MaxUses
	if h.lifetime.isClosing() {
		return nil, ErrClosed
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

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (h *Host) RegisterDriver(driver Driver) error {
	if nilValue(driver) {
		return ErrInvalid
	}
	profile := driver.Profile()
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile == NativeGo {
		return fmt.Errorf("%w: native setup belongs to its declaration", ErrInvalid)
	}
	var realmBound bool
	if bound, ok := driver.(RealmDriver); ok {
		realmBound = bound.RealmBound()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() {
		return ErrClosed
	}
	if _, exists := h.drivers[profile]; exists {
		return fmt.Errorf("%w: duplicate execution profile", ErrInvalid)
	}
	h.drivers[profile] = registeredDriver{Driver: driver, realmBound: realmBound}
	return nil
}

func (h *Host) Register(declarations ...Declaration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() {
		return ErrClosed
	}
	names := make(map[nominalContract]ContractRef, len(declarations))
	definitions := make(map[ContractRef]*declaration, len(declarations))
	for _, token := range declarations {
		if nilValue(token) || token.definition() == nil {
			return ErrInvalid
		}
		definition := token.definition()
		name := contractName(definition.ref)
		if previous, exists := h.contractNames[name]; exists && previous != definition.ref {
			return ErrContractMismatch
		}
		if previous := h.contracts[definition.ref]; previous != nil && !sameDeclaration(previous, definition) {
			return ErrContractMismatch
		}
		if previous, exists := names[name]; exists && previous != definition.ref {
			return ErrContractMismatch
		}
		if previous := definitions[definition.ref]; previous != nil && !sameDeclaration(previous, definition) {
			return ErrContractMismatch
		}
		names[name] = definition.ref
		definitions[definition.ref] = definition
	}
	for ref, definition := range definitions {
		h.contracts[ref] = definition
		h.contractNames[contractName(ref)] = ref
	}
	return nil
}

// RegisterDescriptor admits a declaration whose native implementation lives in
// another execution realm. That realm validates its local language value type.
func (h *Host) RegisterDescriptor(ref ContractRef, descriptor []byte) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	var definition *declaration
	if ref.Kind == PointKind && ref.Mode == PortableMode {
		var err error
		definition, err = pointDeclaration(ref, descriptor)
		if err != nil {
			return err
		}
	} else {
		if framevalue.Hash(descriptor) != ref.Digest {
			return ErrContractMismatch
		}
		definition = &declaration{ref: ref, descriptor: bytes.Clone(descriptor)}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() {
		return ErrClosed
	}
	name := contractName(ref)
	if previous, exists := h.contractNames[name]; exists && previous != ref {
		return ErrContractMismatch
	}
	if old := h.contracts[ref]; old != nil {
		if !bytes.Equal(old.descriptor, descriptor) {
			return ErrContractMismatch
		}
		return nil
	}
	h.contractNames[name] = ref
	h.contracts[ref] = definition
	return nil
}

func (h *Host) ValueContract(ref ContractRef) (ContractRef, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	definition := h.contracts[ref]
	if definition == nil {
		return ContractRef{}, ErrContractMismatch
	}
	if ref.Kind == ServiceKind {
		return ref, nil
	}
	if ref.Kind != PointKind || definition.valueContract == (ContractRef{}) {
		return ContractRef{}, ErrContractMismatch
	}
	if h.contracts[definition.valueContract] == nil {
		return ContractRef{}, ErrContractMismatch
	}
	return definition.valueContract, nil
}

func (h *Host) ContractDescriptor(ref ContractRef) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	definition := h.contracts[ref]
	if definition == nil {
		return nil, ErrContractMismatch
	}
	return bytes.Clone(definition.descriptor), nil
}

// Update admits one compare-and-swap replacement of installation authority.
// Consumer configuration is prepared before this call; the original record is
// checked again under the same admission as retained-use and commit checks.
func (h *Host) Update(ctx context.Context, expected InstallationRecord, config InstallationConfig) error {
	next, err := recordFromConfig(config)
	if err != nil {
		return err
	}
	if next.ID != expected.ID {
		return ErrInvalid
	}
	return h.changeInstallation(ctx, next.ID, func(current *installed) (*installed, error) {
		if !sameInstallationRecord(current.record, expected) {
			return nil, ErrStaleBinding
		}
		return &installed{record: next, plugin: config.Plugin}, nil
	})
}

func sameInstallationRecord(a, b InstallationRecord) bool {
	return a.ID == b.ID && a.Release == b.Release && a.Enabled == b.Enabled && bytes.Equal(a.Configuration, b.Configuration) && sameUnitDeclarations(a.Units, b.Units)
}

func (h *Host) commitInstallation(ctx context.Context, before, after *InstallationRecord) (CommitState, error) {
	if h.store == nil {
		return CommitSucceeded, nil
	}
	change := InstallationChange{}
	if before != nil {
		copy := before.Clone()
		change.Before = &copy
	}
	if after != nil {
		copy := after.Clone()
		change.After = &copy
	}
	state, err := h.store.CommitInstallation(ctx, change)
	switch state {
	case CommitSucceeded:
		return CommitSucceeded, err
	case CommitRejected:
		if err == nil {
			err = fmt.Errorf("%w: installation commit rejected", ErrUnavailable)
		}
		return CommitRejected, err
	default:
		h.admission.RequireRecovery(errors.Join(ErrRecoveryRequired, err))
		return CommitIndeterminate, errors.Join(ErrRecoveryRequired, err)
	}
}

func (h *Host) confirmInstallation(ctx context.Context, id InstallationID) error {
	if h.store == nil {
		return nil
	}
	if err := h.store.ConfirmInstallation(ctx, id); err != nil {
		h.admission.RequireRecovery(err)
		return errors.Join(ErrRecoveryRequired, err)
	}
	return nil
}

func installationScope(id InstallationID) scopeKey {
	return scopeKey{id: ScopeID(id), installation: true}
}

func recordFromConfig(config InstallationConfig) (InstallationRecord, error) {
	if err := config.ID.Validate(); err != nil {
		return InstallationRecord{}, err
	}
	if config.Plugin == nil {
		return InstallationRecord{}, ErrInvalid
	}
	return InstallationRecord{ID: config.ID, Release: config.Plugin.revision, Configuration: bytes.Clone(config.Configuration), Enabled: config.Enabled, Units: config.Plugin.Units()}, nil
}

func (h *Host) Install(ctx context.Context, config InstallationConfig) (InstallationRecord, error) {
	record, err := recordFromConfig(config)
	if err != nil {
		return InstallationRecord{}, err
	}
	state := CommitRejected
	err = h.admitInstallation(ctx, func(admitted context.Context) error {
		h.mu.Lock()
		if h.lifetime.isClosing() {
			h.mu.Unlock()
			return ErrClosed
		}
		if _, found := h.installations[record.ID]; found {
			h.mu.Unlock()
			return fmt.Errorf("%w: installation already exists", ErrInvalid)
		}
		if len(h.installations) >= h.maxInstallations {
			h.mu.Unlock()
			return ErrCapacity
		}
		if err := h.checkReleaseLocked(record); err != nil {
			h.mu.Unlock()
			return err
		}
		if err := h.validateUnitsLocked(record.Units); err != nil {
			h.mu.Unlock()
			return err
		}
		h.mu.Unlock()
		if record.Enabled {
			if err := h.admission.checkScopeCapacity(installationScope(record.ID)); err != nil {
				return err
			}
		}
		var commitErr error
		state, commitErr = h.commitInstallation(admitted, nil, &record)
		if state != CommitSucceeded {
			return commitErr
		}
		entry := &installed{record: record.Clone(), plugin: config.Plugin}
		if record.Enabled {
			entry.scope, err = h.admission.openScope(installationScope(record.ID))
			if err != nil {
				h.admission.RequireRecovery(err)
				return errors.Join(ErrRecoveryRequired, commitErr, err)
			}
		}
		h.mu.Lock()
		h.installations[record.ID] = entry
		h.mu.Unlock()
		h.admission.recordChange(record.ID)
		return errors.Join(commitErr, h.confirmInstallation(admitted, record.ID))
	})
	if state != CommitSucceeded {
		return InstallationRecord{}, installationFailure(config.ID, state, err)
	}
	return record.Clone(), installationFailure(config.ID, state, err)
}

func (h *Host) checkReleaseLocked(record InstallationRecord) error {
	for _, original := range h.installations {
		if original.record.Release == record.Release && !sameUnitDeclarations(original.record.Units, record.Units) {
			return fmt.Errorf("%w: release declarations changed", ErrContractMismatch)
		}
	}
	return nil
}

// The close outcome must include control transactions already admitted, even
// if they have yet to create a resource or to publish their local projection.
func (h *Host) admitInstallation(ctx context.Context, apply func(context.Context) error) error {
	return h.admission.Admit(ctx, func(admitted context.Context) error {
		work, err := h.lifetime.Enter(admitted)
		if err != nil {
			return err
		}
		defer work.Finish()
		return apply(admitted)
	})
}

func (h *Host) validateUnitsLocked(units []UnitDeclaration) error {
	for _, unit := range units {
		if unit.Profile != NativeGo && h.drivers[unit.Profile].Driver == nil {
			return fmt.Errorf("%w: execution profile %s", ErrUnavailable, unit.Profile)
		}
		for _, export := range unit.Exports {
			if h.contracts[export.Contract] == nil {
				return fmt.Errorf("%w: export contract %s not registered", ErrContractMismatch, export.Contract)
			}
		}
		for _, imp := range unit.Imports {
			if h.contracts[imp.Contract] == nil {
				return fmt.Errorf("%w: import contract %s not registered", ErrContractMismatch, imp.Contract)
			}
		}
	}
	return nil
}

func (h *Host) Installation(id InstallationID) (InstallationRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.installations[id]
	if entry == nil {
		return InstallationRecord{}, false
	}
	return entry.record.Clone(), true
}

func (h *Host) SetEnabled(ctx context.Context, id InstallationID, enabled bool) error {
	return h.changeInstallation(ctx, id, func(current *installed) (*installed, error) {
		if current.record.Enabled == enabled {
			return current, nil
		}
		record := current.record.Clone()
		record.Enabled = enabled
		return &installed{record: record, plugin: current.plugin}, nil
	})
}

func (h *Host) Replace(ctx context.Context, id InstallationID, plugin *Plugin, configuration []byte) error {
	if plugin == nil {
		return ErrInvalid
	}
	configuration = bytes.Clone(configuration)
	return h.changeInstallation(ctx, id, func(current *installed) (*installed, error) {
		return &installed{record: InstallationRecord{ID: id, Release: plugin.revision, Configuration: configuration, Enabled: current.record.Enabled, Units: plugin.Units()}, plugin: plugin}, nil
	})
}

func (h *Host) Uninstall(ctx context.Context, id InstallationID) error {
	return h.changeInstallation(ctx, id, func(*installed) (*installed, error) { return nil, nil })
}

type installationTransition struct {
	before *installed
	after  *installed
}

func (t installationTransition) destructive() bool {
	return t.after == nil || t.after.record.Release != t.before.record.Release || !bytes.Equal(t.after.record.Configuration, t.before.record.Configuration)
}

func (t installationTransition) records() (*InstallationRecord, *InstallationRecord) {
	before := t.before.record.Clone()
	if t.after == nil {
		return &before, nil
	}
	after := t.after.record.Clone()
	return &before, &after
}

func (h *Host) changeInstallation(ctx context.Context, id InstallationID, prepare func(*installed) (*installed, error)) error {
	if err := id.Validate(); err != nil {
		return err
	}
	state := CommitRejected
	var retire []*Instance
	err := h.admitInstallation(ctx, func(admitted context.Context) error {
		transition, err := h.planInstallationTransition(id, prepare)
		if err != nil || transition == nil {
			return err
		}
		next := transition.after
		if next != nil && next.record.Enabled {
			if err := h.admission.checkScopeCapacity(installationScope(id)); err != nil {
				return err
			}
		}
		if transition.destructive() {
			if err := h.admission.CheckUnused(admitted, id); err != nil {
				return err
			}
		}
		before, after := transition.records()
		var commitErr error
		state, commitErr = h.commitInstallation(admitted, before, after)
		if state != CommitSucceeded {
			return commitErr
		}
		h.admission.fenceScope(installationScope(id))
		if next != nil && next.record.Enabled {
			next.scope, err = h.admission.openScope(installationScope(id))
			if err != nil {
				h.admission.RequireRecovery(err)
				return errors.Join(ErrRecoveryRequired, commitErr, err)
			}
		}
		retire = h.applyInstallationTransition(id, next)
		h.admission.recordChange(id)
		return errors.Join(commitErr, h.confirmInstallation(admitted, id))
	})
	for _, instance := range retire {
		instance.beginRetire()
	}
	return installationFailure(id, state, err)
}

func (h *Host) planInstallationTransition(id InstallationID, prepare func(*installed) (*installed, error)) (*installationTransition, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.isClosing() {
		return nil, ErrClosed
	}
	current := h.installations[id]
	if current == nil {
		return nil, ErrUnavailable
	}
	next, err := prepare(current)
	if err != nil {
		return nil, err
	}
	if next == current || next != nil && sameInstallationRecord(current.record, next.record) {
		return nil, nil
	}
	if next != nil {
		if err := h.checkReleaseLocked(next.record); err != nil {
			return nil, err
		}
	}
	transition := &installationTransition{before: current, after: next}
	if transition.destructive() {
		for _, use := range h.uses {
			if use.binding.requiresInstallation(id) {
				return nil, ErrInUse
			}
		}
	}
	if next != nil {
		if err := h.validateUnitsLocked(next.record.Units); err != nil {
			return nil, err
		}
	}
	return transition, nil
}

func (h *Host) applyInstallationTransition(id InstallationID, next *installed) []*Instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	if next == nil {
		delete(h.installations, id)
	} else {
		h.installations[id] = next
	}
	var retire []*Instance
	for ref, instance := range h.instances {
		if !instance.snapshot.requiresInstallation(id) {
			continue
		}
		instance.retiring = true
		h.withdrawInstanceLocked(instance)
		delete(h.instances, ref)
		retire = append(retire, instance)
	}
	return retire
}

func (h *Host) Observations() []InstanceObservation {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]InstanceObservation, 0, len(h.allInstances))
	for _, instance := range h.allInstances {
		state := Preparing
		if instance.readyDone {
			state = Ready
		}
		if instance.retiring {
			state = Draining
		}
		instance.lifetime.mu.Lock()
		operation := instance.lifetime.closing
		instance.lifetime.mu.Unlock()
		var closeOutcome *CloseOutcome
		if operation != nil {
			outcome := operation.Outcome()
			closeOutcome = &outcome
			state = Closing
			if outcome.Released {
				state = Closed
			} else if len(outcome.Failures) > 0 {
				state = CloseFailed
			}
		}
		result = append(result, InstanceObservation{
			ID:         instance.id,
			Generation: instance.generation,
			Source:     instance.binding.Source,
			State:      state,
			Failure:    instance.readyErr,
			Close:      closeOutcome,
		})
	}
	slices.SortFunc(result, func(a, b InstanceObservation) int { return compareUnit(a.Source, b.Source) })
	return result
}

func (h *Host) Close(ctx context.Context) (CloseOutcome, error) {
	causes := h.lifetime.ObserveCloseCauses()
	operation := h.lifetime.BeginClose(ErrClosed)
	h.mu.Lock()
	for _, instance := range h.allInstances {
		instance.retiring = true
		h.withdrawInstanceLocked(instance)
	}
	for watch := range h.watches {
		watch.closeLocked(ErrClosed)
	}
	h.mu.Unlock()
	outcome, err := operation.Wait(ctx)
	return outcome, causes.join(err)
}
