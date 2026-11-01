package frame

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	framevalue "github.com/Tangerg/frame/value"
)

// UseOwner reads the original business records. It must use a current committed
// view, never a replica or a separate reference-count ledger. It is trusted
// storage integration, not a callback into plugin code.
type UseOwner interface {
	ID() string
	Held(context.Context, InstallationID) ([]Dependency, error)
}

// RetainedDependency identifies the original business owner that still requires
// an exact release. It is a fresh read projection, not another retention record.
type RetainedDependency struct {
	OwnerID    string
	Dependency Dependency
}

type AdmissionLimits struct {
	MaxPreparations       int
	MaxPreparationChanges int
	MaxScopes             int
	MaxUseOwners          int
}

// Admission serializes new uses with changes and bounded storage commits. Its
// state mutex is never held during storage, execution or consumer code.
type Admission struct {
	slot         chan struct{}
	mu           sync.Mutex
	preparations map[*Preparation]struct{}
	owners       map[string]UseOwner
	scopes       map[scopeKey]scopeState
	limits       AdmissionLimits
	recovery     error
	reports      uint64
	barrier      uint64
	revision     uint64
}

type Preparation struct {
	owner           *Admission
	changed         map[InstallationID]struct{}
	changesExceeded bool
}

// A recovery report observes the original fence. Additional reports are a
// count, not retained error graphs or another ledger of unsettled operations.
type recoveryReport struct {
	cause      error
	additional uint64
}

func (r *recoveryReport) Error() string {
	const maxDetailBytes = 2048
	detail := r.cause.Error()
	if len(detail) > maxDetailBytes {
		detail = detail[:maxDetailBytes]
	}
	text := ErrRecoveryRequired.Error() + ": " + detail
	if r.additional != 0 {
		text += fmt.Sprintf("; %d additional recovery reports", r.additional)
	}
	return text
}

func (r *recoveryReport) Unwrap() []error { return []error{ErrRecoveryRequired, r.cause} }

func (a *Admission) recoveryErrorLocked() error {
	if a.recovery == nil {
		return nil
	}
	return &recoveryReport{cause: a.recovery, additional: a.reports}
}

type scopeKey struct {
	id           ScopeID
	installation bool
}

type scopeState struct {
	context    context.Context
	cancel     context.CancelFunc
	generation uint64
	open       bool
}

type ScopeObservation struct {
	ID         ScopeID
	Generation uint64
	Fenced     bool
}

// ScopeGrant is an approval of a particular scope generation. Copying the value
// does not create permission to reopen that scope after it has been fenced.
type ScopeGrant struct {
	context    context.Context
	owner      *Admission
	key        scopeKey
	generation uint64
}

type admissionContextKey struct{}
type admissionContext struct {
	owner  *Admission
	active atomic.Bool
}

func (g ScopeGrant) ID() ScopeID { return g.key.id }

// WireID identifies the scope independently of its generation. A tenant's
// chosen name cannot collide with the installation namespace on a remote Realm.
func (g ScopeGrant) WireID() ScopeID {
	if g.owner == nil {
		return ""
	}
	namespace := "application/"
	if g.key.installation {
		namespace = "installation/"
	}
	return ScopeID(namespace + framevalue.Hash([]byte(g.key.id)).String())
}
func (g ScopeGrant) Generation() uint64 { return g.generation }

func NewAdmission() *Admission {
	admission, err := NewAdmissionWithLimits(AdmissionLimits{})
	if err != nil {
		panic(err)
	}
	return admission
}

// NewAdmissionWithLimits bounds original preparation, scope and business-owner
// records. A closed scope retains its generation and still occupies capacity.
func NewAdmissionWithLimits(limits AdmissionLimits) (*Admission, error) {
	if limits.MaxPreparations == 0 {
		limits.MaxPreparations = 4096
	}
	if limits.MaxPreparationChanges == 0 {
		limits.MaxPreparationChanges = 4096
	}
	if limits.MaxScopes == 0 {
		limits.MaxScopes = 4096
	}
	if limits.MaxUseOwners == 0 {
		limits.MaxUseOwners = 256
	}
	if limits.MaxPreparations < 1 || limits.MaxPreparationChanges < 1 || limits.MaxScopes < 1 || limits.MaxUseOwners < 1 {
		return nil, ErrInvalid
	}
	return &Admission{
		slot:         make(chan struct{}, 1),
		preparations: make(map[*Preparation]struct{}),
		owners:       make(map[string]UseOwner),
		scopes:       make(map[scopeKey]scopeState),
		limits:       limits,
	}, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	return nil
}

func (a *Admission) enter(ctx context.Context, maintenance bool) (func(), error) {
	if ctx == nil || a == nil {
		return nil, ErrInvalid
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	select {
	case a.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, contextError(ctx)
	}
	leave := func() { <-a.slot }
	if err := contextError(ctx); err != nil {
		leave()
		return nil, err
	}
	if !maintenance {
		a.mu.Lock()
		err := a.recoveryErrorLocked()
		a.mu.Unlock()
		if err != nil {
			leave()
			return nil, err
		}
	}
	return leave, nil
}

// Under accepts trusted bounded work. It must not invoke a plugin, perform
// network I/O or reenter this admission boundary.
func (a *Admission) Under(ctx context.Context, apply func() error) error {
	if apply == nil {
		return ErrInvalid
	}
	return a.Admit(ctx, func(context.Context) error { return apply() })
}

// Admit supplies proof of this exact bounded admission to trusted storage and
// binding validation. A retained context stops authorizing work when it returns.
func (a *Admission) Admit(ctx context.Context, apply func(context.Context) error) error {
	if apply == nil {
		return ErrInvalid
	}
	leave, err := a.enter(ctx, false)
	if err != nil {
		return err
	}
	defer leave()
	proof := &admissionContext{owner: a}
	proof.active.Store(true)
	defer proof.active.Store(false)
	return apply(context.WithValue(ctx, admissionContextKey{}, proof))
}

func (a *Admission) CheckAdmitted(ctx context.Context) error {
	proof, ok := ctx.Value(admissionContextKey{}).(*admissionContext)
	if !ok || proof.owner != a || !proof.active.Load() {
		return fmt.Errorf("%w: active shared admission is required", ErrInvalid)
	}
	return nil
}

// Maintenance uses the same gate while recovery is required. Its caller may
// only settle original operations, seal authority or release existing resources.
func (a *Admission) Maintenance(ctx context.Context, apply func() error) error {
	if apply == nil {
		return ErrInvalid
	}
	leave, err := a.enter(ctx, true)
	if err != nil {
		return err
	}
	defer leave()
	return apply()
}

func (a *Admission) Begin() (*Preparation, func(), error) {
	a.mu.Lock()
	if a.recovery != nil {
		err := a.recoveryErrorLocked()
		a.mu.Unlock()
		return nil, nil, err
	}
	if len(a.preparations) >= a.limits.MaxPreparations {
		a.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: preparations", ErrCapacity)
	}
	p := &Preparation{owner: a, changed: make(map[InstallationID]struct{})}
	a.preparations[p] = struct{}{}
	a.mu.Unlock()
	var once sync.Once
	abandon := func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.preparations, p)
			p.changed = nil
			a.mu.Unlock()
		})
	}
	return p, abandon, nil
}

func (a *Admission) Publish(ctx context.Context, p *Preparation, dependencies []InstallationID, publish func() error) error {
	if p == nil || p.owner != a || publish == nil {
		return ErrInvalid
	}
	return a.Under(ctx, func() error {
		a.mu.Lock()
		_, active := a.preparations[p]
		delete(a.preparations, p)
		exceeded := p.changesExceeded
		var changed InstallationID
		for _, id := range dependencies {
			if _, ok := p.changed[id]; ok {
				changed = id
				break
			}
		}
		p.changed = nil
		a.mu.Unlock()
		if !active {
			return fmt.Errorf("%w: preparation is no longer active", ErrStaleBinding)
		}
		if exceeded {
			return fmt.Errorf("%w: preparation change history exceeds %d installations: %w", ErrStaleBinding, a.limits.MaxPreparationChanges, ErrCapacity)
		}
		if changed != "" {
			return fmt.Errorf("%w: installation %s changed during preparation", ErrStaleBinding, changed)
		}
		return publish()
	})
}

func (a *Admission) recordChange(id InstallationID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revision++
	for p := range a.preparations {
		if p.changesExceeded {
			continue
		}
		if _, exists := p.changed[id]; exists {
			continue
		}
		if len(p.changed) == a.limits.MaxPreparationChanges {
			// Committed changes remain facts. Only this unfinished preparation
			// loses permission to publish when its bounded read history is full.
			p.changesExceeded = true
			continue
		}
		p.changed[id] = struct{}{}
	}
}

// Change records a definite committed transition even when its subsequent
// local projection reports an error. An indeterminate commit must first call
// RequireRecovery; it must not report changed=false as proof of rollback.
func (a *Admission) Change(ctx context.Context, id InstallationID, apply func() (bool, error)) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if apply == nil {
		return ErrInvalid
	}
	return a.Under(ctx, func() error {
		changed, err := apply()
		if changed {
			a.recordChange(id)
		}
		return err
	})
}

func (a *Admission) Revision() uint64 { a.mu.Lock(); defer a.mu.Unlock(); return a.revision }

func (a *Admission) RequireRecovery(cause error) {
	if cause == nil {
		cause = ErrRecoveryRequired
	}
	a.mu.Lock()
	if a.recovery == nil {
		a.recovery = cause
	} else if a.reports != ^uint64(0) {
		a.reports++
	}
	a.barrier++
	a.mu.Unlock()
}

func (a *Admission) RecoveryError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recoveryErrorLocked()
}

// Recover only clears the barrier it observed. A new failure raised during
// restoration cannot be erased by an older recovery completing late.
func (a *Admission) Recover(ctx context.Context, restore func() error) error {
	if restore == nil {
		return ErrInvalid
	}
	return a.Maintenance(ctx, func() error {
		a.mu.Lock()
		barrier := a.barrier
		a.mu.Unlock()
		if err := restore(); err != nil {
			a.RequireRecovery(err)
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.barrier != barrier {
			return a.recoveryErrorLocked()
		}
		a.recovery = nil
		a.reports = 0
		return nil
	})
}

func (a *Admission) RegisterUseOwner(owner UseOwner) error {
	return a.registerUseOwners([]UseOwner{owner})
}

func (a *Admission) registerUseOwners(owners []UseOwner) error {
	pending := make(map[string]UseOwner, len(owners))
	for _, owner := range owners {
		if nilValue(owner) {
			return ErrInvalid
		}
		id := owner.ID()
		if !ValidName(id) {
			return ErrInvalid
		}
		if _, exists := pending[id]; exists {
			return fmt.Errorf("%w: duplicate use owner %s", ErrInvalid, id)
		}
		pending[id] = owner
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range pending {
		if _, exists := a.owners[id]; exists {
			return fmt.Errorf("%w: duplicate use owner %s", ErrInvalid, id)
		}
	}
	if len(a.owners)+len(pending) > a.limits.MaxUseOwners {
		return fmt.Errorf("%w: use owners", ErrCapacity)
	}
	for id, owner := range pending {
		a.owners[id] = owner
	}
	return nil
}

// Retained performs fresh reads without entering the gate again. Destructive
// commands call it from admission; diagnostic reads may call it directly.
// An unavailable owner cannot prove the absence of retained dependencies.
func (a *Admission) Retained(ctx context.Context, id InstallationID) ([]RetainedDependency, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	a.mu.Lock()
	ids := make([]string, 0, len(a.owners))
	for id := range a.owners {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	owners := make([]UseOwner, 0, len(ids))
	for _, id := range ids {
		owners = append(owners, a.owners[id])
	}
	a.mu.Unlock()
	var result []RetainedDependency
	for index, owner := range owners {
		ownerID := ids[index]
		values, err := owner.Held(ctx, id)
		if err != nil {
			failure := fmt.Errorf("frame: read retained dependencies from %s: %w", ownerID, err)
			if onlyRequestCancellation(ctx, err) {
				return nil, failure
			}
			a.RequireRecovery(failure)
			return nil, errors.Join(ErrRecoveryRequired, failure)
		}
		for _, value := range values {
			if err := value.Validate(); err != nil {
				failure := fmt.Errorf("frame: invalid retained dependency from %s: %w", ownerID, err)
				a.RequireRecovery(failure)
				return nil, errors.Join(ErrRecoveryRequired, failure)
			}
			if value.InstallationID != id {
				failure := fmt.Errorf("%w: use owner %s returned another installation", ErrInvalid, ownerID)
				a.RequireRecovery(failure)
				return nil, errors.Join(ErrRecoveryRequired, failure)
			}
		}
		for _, value := range CompactDependencies(values) {
			result = append(result, RetainedDependency{OwnerID: ownerID, Dependency: value})
		}
	}
	return result, nil
}

// Held projects the distinct dependencies from the same original owner reads.
func (a *Admission) Held(ctx context.Context, id InstallationID) ([]Dependency, error) {
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
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyRequestCancellation(ctx, cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyRequestCancellation(ctx, value.Unwrap())
	default:
		return errors.Is(ctx.Err(), err) || errors.Is(context.Cause(ctx), err)
	}
}

func (a *Admission) CheckUnused(ctx context.Context, id InstallationID) error {
	values, err := a.Held(ctx, id)
	if err != nil {
		return err
	}
	if len(values) != 0 {
		return fmt.Errorf("%w: %s", ErrInUse, id)
	}
	return nil
}

func (a *Admission) OpenScope(ctx context.Context, id ScopeID) (ScopeGrant, error) {
	if err := id.Validate(); err != nil {
		return ScopeGrant{}, err
	}
	var grant ScopeGrant
	err := a.Under(ctx, func() error {
		var err error
		grant, err = a.openScope(scopeKey{id: id})
		return err
	})
	return grant, err
}

// Scope allocation is checked before a durable installation mutation. The
// caller holds the shared gate, which also serializes every scope allocation.
func (a *Admission) checkScopeCapacity(ids ...scopeKey) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	missing := make(map[scopeKey]struct{})
	for _, id := range ids {
		if _, exists := a.scopes[id]; !exists {
			missing[id] = struct{}{}
		}
	}
	if len(a.scopes)+len(missing) > a.limits.MaxScopes {
		return fmt.Errorf("%w: scopes", ErrCapacity)
	}
	return nil
}

func (a *Admission) openScope(id scopeKey) (ScopeGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[id]
	if !exists && len(a.scopes) >= a.limits.MaxScopes {
		return ScopeGrant{}, fmt.Errorf("%w: scopes", ErrCapacity)
	}
	if !state.open {
		state.generation++
		state.context, state.cancel = context.WithCancel(context.Background())
		state.open = true
		a.scopes[id] = state
	}
	return ScopeGrant{owner: a, key: id, generation: state.generation, context: state.context}, nil
}

// Restoring committed enabled state cannot revoke a later local fence. A cold
// scope has no earlier grant to preserve, so only that case starts a generation.
func (a *Admission) restoreScope(id scopeKey) (ScopeGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[id]
	if !exists {
		if len(a.scopes) >= a.limits.MaxScopes {
			return ScopeGrant{}, fmt.Errorf("%w: scopes", ErrCapacity)
		}
		state.generation++
		state.context, state.cancel = context.WithCancel(context.Background())
		state.open = true
		a.scopes[id] = state
	}
	return ScopeGrant{owner: a, key: id, generation: state.generation, context: state.context}, nil
}

// FenceScope is a local monotonic action and never waits for storage or the
// admission slot. Unknown scopes have no grants to revoke and allocate no
// record. Fencing does not claim remote work has finished.
func (a *Admission) FenceScope(id ScopeID) { a.fenceScope(scopeKey{id: id}) }

func (a *Admission) fenceScope(id scopeKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[id]
	if !exists {
		return
	}
	state.open = false
	state.cancel()
	a.scopes[id] = state
}

// Scope observes the original generation and fence even while recovery blocks
// new admission. The observation does not grant authority to use or reopen it.
func (a *Admission) Scope(id ScopeID) (ScopeObservation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[scopeKey{id: id}]
	if !exists {
		return ScopeObservation{}, false
	}
	return ScopeObservation{ID: id, Generation: state.generation, Fenced: !state.open}, true
}

func (a *Admission) ValidateScope(grant ScopeGrant) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.scopes[grant.key]
	if grant.owner != a || !state.open || state.generation != grant.generation {
		return ErrFenced
	}
	return nil
}
