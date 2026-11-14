package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// UseOwner reads the original business records. It must use a current committed
// view, never a replica or a separate reference-count ledger. It is trusted
// storage integration, not a callback into plugin code.
type UseOwner interface {
	ID() string
	Held(context.Context, InstallationID) ([]Dependency, error)
}

type registeredUseOwner struct {
	id     string
	source UseOwner
}

// RetainedDependency identifies the original business owner that still requires
// an exact release. It is a fresh read projection, not another retention record.
type RetainedDependency struct {
	OwnerID    string
	Dependency Dependency
}

type AdmissionLimits struct {
	MaxScopes    int
	MaxUseOwners int
}

const (
	defaultMaxScopes    = 4096
	defaultMaxUseOwners = 256
)

// Admission serializes new uses with changes and bounded storage commits. Its
// state mutex is never held during storage, execution or consumer code.
type Admission struct{ *admissionState }

type admissionState struct {
	slot   chan struct{}
	limits AdmissionLimits

	mu           sync.Mutex
	owners       map[string]registeredUseOwner
	ownersSealed bool
	scopes       map[scopeKey]*scopeState
	recovery     *recoveryReport
}

// Reports are immutable recovery identities. Additional reports retain the
// original cause, so old observations cannot retain later error graphs.
type recoveryReport struct {
	cause      error
	additional uint64
}

func (r *recoveryReport) Error() string {
	text := ErrRecoveryRequired.Error() + ": " + frame.BoundedDiagnostic(r.cause.Error())
	if r.additional != 0 {
		text += fmt.Sprintf("; %d additional recovery reports", r.additional)
	}
	return text
}

func (r *recoveryReport) Unwrap() []error { return []error{ErrRecoveryRequired, r.cause} }

type admissionContextKey struct{}

// admissionProof marks a context derived inside one Admit call. It stops
// authorizing work as soon as that call returns.
type admissionProof struct {
	owner  *admissionState
	active atomic.Bool
}

func NewAdmission() Admission {
	admission, err := NewAdmissionWithLimits(AdmissionLimits{})
	if err != nil {
		panic(err)
	}
	return admission
}

// NewAdmissionWithLimits bounds scope and business-owner records. A fenced
// scope retains its generation and still occupies capacity.
func NewAdmissionWithLimits(limits AdmissionLimits) (Admission, error) {
	var err error
	if limits.MaxScopes, err = frame.ResolveLimit("scopes", limits.MaxScopes, defaultMaxScopes); err != nil {
		return Admission{}, err
	}
	if limits.MaxUseOwners, err = frame.ResolveLimit("use owners", limits.MaxUseOwners, defaultMaxUseOwners); err != nil {
		return Admission{}, err
	}
	return Admission{&admissionState{
		slot:   make(chan struct{}, 1),
		limits: limits,
		owners: make(map[string]registeredUseOwner),
		scopes: make(map[scopeKey]*scopeState),
	}}, nil
}

// acquire takes the single admission slot; the caller must call the returned
// release exactly once.
func (a *admissionState) acquire(ctx context.Context) (func(), error) {
	if a == nil {
		return nil, frame.ErrInvalid
	}
	if err := frame.ContextError(ctx); err != nil {
		return nil, err
	}
	select {
	case a.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, frame.ContextError(ctx)
	}
	release := func() { <-a.slot }
	if err := frame.ContextError(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Under accepts trusted bounded work. It must not invoke a plugin, perform
// network I/O or reenter this admission boundary.
func (a *admissionState) Under(ctx context.Context, apply func() error) error {
	if apply == nil {
		return frame.ErrInvalid
	}
	return a.Admit(ctx, func(context.Context) error { return apply() })
}

// Admit supplies proof of this exact bounded admission to trusted storage and
// binding validation. A retained context stops authorizing work when it returns.
func (a *admissionState) Admit(ctx context.Context, apply func(context.Context) error) error {
	if apply == nil {
		return frame.ErrInvalid
	}
	release, err := a.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := a.RecoveryError(); err != nil {
		return err
	}
	proof := &admissionProof{owner: a}
	proof.active.Store(true)
	defer proof.active.Store(false)
	return apply(context.WithValue(ctx, admissionContextKey{}, proof))
}

func (a *admissionState) CheckAdmitted(ctx context.Context) error {
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	proof, ok := ctx.Value(admissionContextKey{}).(*admissionProof)
	if !ok || proof.owner != a || !proof.active.Load() {
		return fmt.Errorf("%w: active shared admission is required", frame.ErrInvalid)
	}
	return nil
}

// Maintenance uses the same gate while recovery is required. Its caller may
// only settle original operations, seal authority or release existing resources.
func (a *admissionState) Maintenance(ctx context.Context, apply func() error) error {
	if apply == nil {
		return frame.ErrInvalid
	}
	release, err := a.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return apply()
}

func (a *admissionState) RequireRecovery(cause error) {
	if cause == nil {
		cause = ErrRecoveryRequired
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next := &recoveryReport{cause: cause}
	if previous := a.recovery; previous != nil {
		next.cause = previous.cause
		next.additional = previous.additional + 1
	}
	a.recovery = next
}

func (a *admissionState) RecoveryError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recoveryErrorLocked()
}

func (a *admissionState) recoveryErrorLocked() error {
	if a.recovery == nil {
		return nil
	}
	return a.recovery
}

// Recover only clears the barrier it observed. A new failure raised during
// restoration cannot be erased by an older recovery completing late.
func (a *admissionState) Recover(ctx context.Context, restore func() error) error {
	if restore == nil {
		return frame.ErrInvalid
	}
	return a.Maintenance(ctx, func() error {
		a.mu.Lock()
		observed := a.recovery
		a.mu.Unlock()
		returned := false
		defer func() {
			if !returned {
				a.RequireRecovery(fmt.Errorf("%w: restoration interrupted", ErrRecoveryRequired))
			}
		}()
		err := restore()
		returned = true
		if err != nil {
			a.RequireRecovery(err)
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.recovery != observed {
			return a.recoveryErrorLocked()
		}
		a.recovery = nil
		return nil
	})
}

func (a *admissionState) RegisterUseOwner(owner UseOwner) error {
	return a.registerUseOwners([]UseOwner{owner})
}

func (a *admissionState) registerUseOwners(owners []UseOwner) error {
	pending := make(map[string]registeredUseOwner, len(owners))
	for _, owner := range owners {
		if framevalue.IsNil(owner) {
			return frame.ErrInvalid
		}
		id := owner.ID()
		if !frame.ValidName(id) {
			return frame.ErrInvalid
		}
		if _, exists := pending[id]; exists {
			return fmt.Errorf("%w: duplicate use owner %s", frame.ErrInvalid, id)
		}
		pending[id] = registeredUseOwner{id: id, source: owner}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(pending) != 0 && a.ownersSealed {
		return fmt.Errorf("%w: business owners already sealed", frame.ErrClosed)
	}
	for id := range pending {
		if _, exists := a.owners[id]; exists {
			return fmt.Errorf("%w: duplicate use owner %s", frame.ErrInvalid, id)
		}
	}
	if len(a.owners)+len(pending) > a.limits.MaxUseOwners {
		return fmt.Errorf("%w: use owners", frame.ErrCapacity)
	}
	for id, owner := range pending {
		a.owners[id] = owner
	}
	return nil
}

// Retained performs fresh reads without entering the gate again. Destructive
// commands call it from admission; diagnostic reads may call it directly.
// An unavailable owner cannot prove the absence of retained dependencies.
func (a *admissionState) Retained(ctx context.Context, id InstallationID) ([]RetainedDependency, error) {
	if err := frame.ContextError(ctx); err != nil {
		return nil, err
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	var result []RetainedDependency
	for _, owner := range a.sortedOwners() {
		values, err := a.held(ctx, owner, id)
		if err != nil {
			return nil, err
		}
		for _, value := range CompactDependencies(values) {
			result = append(result, RetainedDependency{OwnerID: owner.id, Dependency: value})
		}
	}
	return result, nil
}

func (a *admissionState) sortedOwners() []registeredUseOwner {
	a.mu.Lock()
	owners := make([]registeredUseOwner, 0, len(a.owners))
	for _, owner := range a.owners {
		owners = append(owners, owner)
	}
	a.mu.Unlock()
	slices.SortFunc(owners, func(x, y registeredUseOwner) int { return cmp.Compare(x.id, y.id) })
	return owners
}

// held reads one owner. Any answer it cannot trust requires recovery, except a
// failure caused only by this request's own cancellation.
func (a *admissionState) held(ctx context.Context, owner registeredUseOwner, id InstallationID) ([]Dependency, error) {
	returned := false
	defer func() {
		if !returned {
			a.RequireRecovery(fmt.Errorf("%w: retained dependency read from %s interrupted", ErrRecoveryRequired, owner.id))
		}
	}()
	values, err := owner.source.Held(ctx, id)
	returned = true
	if err != nil {
		failure := fmt.Errorf("host: read retained dependencies from %s: %w", owner.id, err)
		if onlyRequestCancellation(ctx, err) {
			return nil, failure
		}
		return nil, a.untrusted(failure)
	}
	for _, value := range values {
		if err := value.Validate(); err != nil {
			return nil, a.untrusted(fmt.Errorf("host: invalid retained dependency from %s: %w", owner.id, err))
		}
		if value.InstallationID != id {
			return nil, a.untrusted(fmt.Errorf("%w: use owner %s returned another installation", frame.ErrInvalid, owner.id))
		}
	}
	return values, nil
}

func (a *admissionState) untrusted(failure error) error {
	a.RequireRecovery(failure)
	return errors.Join(ErrRecoveryRequired, failure)
}

// Held projects the distinct dependencies from the same original owner reads.
func (a *admissionState) Held(ctx context.Context, id InstallationID) ([]Dependency, error) {
	retained, err := a.Retained(ctx, id)
	if err != nil {
		return nil, err
	}
	values := make([]Dependency, 0, len(retained))
	for _, value := range retained {
		values = append(values, value.Dependency)
	}
	return CompactDependencies(values), nil
}

// Cancellation can accompany a real storage failure. Only an error composed
// entirely of this request's cancellation is safe to leave outside recovery.
func onlyRequestCancellation(ctx context.Context, err error) bool {
	if ctx.Err() == nil || err == nil {
		return false
	}
	switch value := err.(type) {
	case interface{ Unwrap() []error }:
		causes := value.Unwrap()
		return len(causes) > 0 && !slices.ContainsFunc(causes, func(cause error) bool { return !onlyRequestCancellation(ctx, cause) })
	case interface{ Unwrap() error }:
		return onlyRequestCancellation(ctx, value.Unwrap())
	default:
		return errors.Is(ctx.Err(), err) || errors.Is(context.Cause(ctx), err)
	}
}

// Destructive decisions rely on the complete owner set. Once one can observe
// absence, a late owner cannot introduce references to an already removed release.
func (a *admissionState) CheckUnused(ctx context.Context, id InstallationID) error {
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	if err := id.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	a.ownersSealed = true
	a.mu.Unlock()
	values, err := a.Held(ctx, id)
	if err != nil {
		return err
	}
	if len(values) != 0 {
		return fmt.Errorf("%w: %s", ErrInUse, id)
	}
	return nil
}

func (a Admission) Valid() bool { return a.admissionState != nil }

// scopeNamespace keeps installation scopes apart from application scopes that
// happen to share a name.
type scopeNamespace uint8

const (
	applicationNamespace scopeNamespace = iota
	installationNamespace
)

func (n scopeNamespace) wirePrefix() string {
	if n == installationNamespace {
		return "installation/"
	}
	return "application/"
}

type scopeKey struct {
	namespace scopeNamespace
	id        ScopeID
}

func applicationScope(id ScopeID) scopeKey { return scopeKey{namespace: applicationNamespace, id: id} }

func installationScope(id InstallationID) scopeKey {
	return scopeKey{namespace: installationNamespace, id: ScopeID(id)}
}

// Every fence replaces this state, including repeated fences of a closed
// generation. A late installation commit may advance only its captured state.
type scopeState struct{ generation *scopeGeneration }

type scopeGeneration struct {
	owner   *admissionState
	key     scopeKey
	number  uint64
	context context.Context
	cancel  context.CancelFunc
}

func (s *scopeState) open() bool {
	return s != nil && s.generation.context.Err() == nil
}

func (a *admissionState) captureScope(key scopeKey) *scopeState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scopes[key]
}

type ScopeObservation struct {
	ID         ScopeID
	Generation uint64
	Fenced     bool
}

// ScopeGrant is an approval of a particular scope generation. Copying the value
// does not create permission to reopen that scope after it has been fenced.
type ScopeGrant struct{ *scopeGeneration }

func (g ScopeGrant) ID() ScopeID {
	if !g.issued() {
		return ""
	}
	return g.key.id
}

func (g ScopeGrant) Generation() uint64 {
	if !g.issued() {
		return 0
	}
	return g.number
}

func (g ScopeGrant) issued() bool { return g.scopeGeneration != nil }

// WireID identifies the scope independently of its generation. A tenant's
// chosen name cannot collide with the installation namespace on a remote Realm.
func (g ScopeGrant) WireID() ScopeID {
	if !g.issued() {
		return ""
	}
	return ScopeID(g.key.namespace.wirePrefix() + framevalue.Hash([]byte(g.key.id)).String())
}

func (a *admissionState) OpenScope(ctx context.Context, id ScopeID) (ScopeGrant, error) {
	if err := id.Validate(); err != nil {
		return ScopeGrant{}, err
	}
	var grant ScopeGrant
	err := a.Under(ctx, func() error {
		var err error
		grant, err = a.openScope(applicationScope(id))
		return err
	})
	return grant, err
}

// checkScopeCapacity runs before a durable installation mutation. The caller
// holds the admission slot, which also serializes every scope allocation.
func (a *admissionState) checkScopeCapacity(keys ...scopeKey) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	missing := make(map[scopeKey]bool, len(keys))
	for _, key := range keys {
		if _, exists := a.scopes[key]; !exists {
			missing[key] = true
		}
	}
	if len(a.scopes)+len(missing) > a.limits.MaxScopes {
		return fmt.Errorf("%w: scopes", frame.ErrCapacity)
	}
	return nil
}

func (a *admissionState) openScope(key scopeKey) (ScopeGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[key]
	if !exists && len(a.scopes) >= a.limits.MaxScopes {
		return ScopeGrant{}, fmt.Errorf("%w: scopes", frame.ErrCapacity)
	}
	if !state.open() {
		state = a.nextGenerationLocked(key, state)
	}
	return ScopeGrant{scopeGeneration: state.generation}, nil
}

// A recovered transition may advance only the scope revision it originally
// admitted. A later fence must survive even when that transition committed.
func (a *admissionState) restoreScope(key scopeKey, admitted *scopeState) (ScopeGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[key]
	if !exists || admitted != nil && state == admitted {
		if !exists && len(a.scopes) >= a.limits.MaxScopes {
			return ScopeGrant{}, fmt.Errorf("%w: scopes", frame.ErrCapacity)
		}
		state = a.nextGenerationLocked(key, state)
	}
	return ScopeGrant{scopeGeneration: state.generation}, nil
}

func (a *admissionState) nextGenerationLocked(key scopeKey, previous *scopeState) *scopeState {
	var number uint64
	if previous != nil {
		previous.generation.cancel()
		number = previous.generation.number
	}
	ctx, cancel := context.WithCancel(context.Background())
	next := &scopeState{generation: &scopeGeneration{owner: a, key: key, number: number + 1, context: ctx, cancel: cancel}}
	a.scopes[key] = next
	return next
}

// FenceScope is a local monotonic action and never waits for storage or the
// admission slot. Unknown scopes have no grants to revoke and allocate no
// record. Fencing does not claim remote work has finished.
func (a *admissionState) FenceScope(id ScopeID) { a.fenceScope(applicationScope(id)) }

func (a *admissionState) fenceScope(key scopeKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if state, exists := a.scopes[key]; exists {
		state.generation.cancel()
		a.scopes[key] = &scopeState{generation: state.generation}
	}
}

// Scope observes the original generation and fence even while recovery blocks
// new admission. The observation does not grant authority to use or reopen it.
func (a *admissionState) Scope(id ScopeID) (ScopeObservation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[applicationScope(id)]
	if !exists {
		return ScopeObservation{}, false
	}
	return ScopeObservation{ID: id, Generation: state.generation.number, Fenced: !state.open()}, true
}

func (a *admissionState) ValidateScope(grant ScopeGrant) error {
	if !grant.issued() {
		return ErrFenced
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.scopes[grant.key]
	if grant.owner != a || !state.open() || state.generation != grant.scopeGeneration {
		return ErrFenced
	}
	return nil
}
