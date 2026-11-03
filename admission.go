package frame

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/internal/nilness"
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
	MaxScopes    int
	MaxUseOwners int
}

const (
	defaultMaxScopes    = 4096
	defaultMaxUseOwners = 256
)

// Admission serializes new uses with changes and bounded storage commits. Its
// state mutex is never held during storage, execution or consumer code.
type Admission struct {
	slot   chan struct{}
	limits AdmissionLimits

	mu           sync.Mutex
	owners       map[string]UseOwner
	ownersSealed bool
	scopes       map[scopeKey]scopeState
	// barrier counts every recovery report. recovery is the first cause of the
	// current barrier, which began at recoveryStart.
	barrier       uint64
	recovery      error
	recoveryStart uint64
}

// recoveryReport observes the original fence. Later reports are only counted,
// so retaining it cannot retain their error graphs.
type recoveryReport struct {
	cause      error
	additional uint64
}

func (r *recoveryReport) Error() string {
	text := ErrRecoveryRequired.Error() + ": " + boundedDetail(r.cause.Error())
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
	owner  *Admission
	active atomic.Bool
}

func NewAdmission() *Admission {
	admission, err := NewAdmissionWithLimits(AdmissionLimits{})
	if err != nil {
		panic(err)
	}
	return admission
}

// NewAdmissionWithLimits bounds scope and business-owner records. A fenced
// scope retains its generation and still occupies capacity.
func NewAdmissionWithLimits(limits AdmissionLimits) (*Admission, error) {
	if err := resolveLimit("scopes", &limits.MaxScopes, defaultMaxScopes); err != nil {
		return nil, err
	}
	if err := resolveLimit("use owners", &limits.MaxUseOwners, defaultMaxUseOwners); err != nil {
		return nil, err
	}
	return &Admission{
		slot:   make(chan struct{}, 1),
		limits: limits,
		owners: make(map[string]UseOwner),
		scopes: make(map[scopeKey]scopeState),
	}, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	return cancellation.Err(ctx)
}

// acquire takes the single admission slot; the caller must call the returned
// release exactly once.
func (a *Admission) acquire(ctx context.Context) (func(), error) {
	if a == nil {
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
	release := func() { <-a.slot }
	if err := contextError(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
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

func (a *Admission) CheckAdmitted(ctx context.Context) error {
	proof, ok := ctx.Value(admissionContextKey{}).(*admissionProof)
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
	release, err := a.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return apply()
}

func (a *Admission) RequireRecovery(cause error) {
	if cause == nil {
		cause = ErrRecoveryRequired
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.barrier++
	if a.recovery == nil {
		a.recovery = cause
		a.recoveryStart = a.barrier
	}
}

func (a *Admission) RecoveryError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recoveryErrorLocked()
}

func (a *Admission) recoveryErrorLocked() error {
	if a.recovery == nil {
		return nil
	}
	return &recoveryReport{cause: a.recovery, additional: a.barrier - a.recoveryStart}
}

// Recover only clears the barrier it observed. A new failure raised during
// restoration cannot be erased by an older recovery completing late.
func (a *Admission) Recover(ctx context.Context, restore func() error) error {
	if restore == nil {
		return ErrInvalid
	}
	return a.Maintenance(ctx, func() error {
		a.mu.Lock()
		observed := a.barrier
		a.mu.Unlock()
		if err := restore(); err != nil {
			a.RequireRecovery(err)
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.barrier != observed {
			return a.recoveryErrorLocked()
		}
		a.recovery = nil
		return nil
	})
}

func (a *Admission) RegisterUseOwner(owner UseOwner) error {
	return a.registerUseOwners([]UseOwner{owner})
}

func (a *Admission) registerUseOwners(owners []UseOwner) error {
	pending := make(map[string]UseOwner, len(owners))
	for _, owner := range owners {
		if nilness.Is(owner) || !ValidName(owner.ID()) {
			return ErrInvalid
		}
		if _, exists := pending[owner.ID()]; exists {
			return fmt.Errorf("%w: duplicate use owner %s", ErrInvalid, owner.ID())
		}
		pending[owner.ID()] = owner
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(pending) != 0 && a.ownersSealed {
		return fmt.Errorf("%w: business owners already sealed", ErrClosed)
	}
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
	if err := contextError(ctx); err != nil {
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
			result = append(result, RetainedDependency{OwnerID: owner.ID(), Dependency: value})
		}
	}
	return result, nil
}

func (a *Admission) sortedOwners() []UseOwner {
	a.mu.Lock()
	defer a.mu.Unlock()
	owners := make([]UseOwner, 0, len(a.owners))
	for _, owner := range a.owners {
		owners = append(owners, owner)
	}
	slices.SortFunc(owners, func(x, y UseOwner) int { return cmp.Compare(x.ID(), y.ID()) })
	return owners
}

// held reads one owner. Any answer it cannot trust requires recovery, except a
// failure caused only by this request's own cancellation.
func (a *Admission) held(ctx context.Context, owner UseOwner, id InstallationID) ([]Dependency, error) {
	values, err := owner.Held(ctx, id)
	if err != nil {
		failure := fmt.Errorf("frame: read retained dependencies from %s: %w", owner.ID(), err)
		if onlyRequestCancellation(ctx, err) {
			return nil, failure
		}
		return nil, a.untrusted(failure)
	}
	for _, value := range values {
		if err := value.Validate(); err != nil {
			return nil, a.untrusted(fmt.Errorf("frame: invalid retained dependency from %s: %w", owner.ID(), err))
		}
		if value.InstallationID != id {
			return nil, a.untrusted(fmt.Errorf("%w: use owner %s returned another installation", ErrInvalid, owner.ID()))
		}
	}
	return values, nil
}

func (a *Admission) untrusted(failure error) error {
	a.RequireRecovery(failure)
	return errors.Join(ErrRecoveryRequired, failure)
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
		return len(causes) > 0 && !slices.ContainsFunc(causes, func(cause error) bool { return !onlyRequestCancellation(ctx, cause) })
	case interface{ Unwrap() error }:
		return onlyRequestCancellation(ctx, value.Unwrap())
	default:
		return errors.Is(ctx.Err(), err) || errors.Is(context.Cause(ctx), err)
	}
}

// Destructive decisions rely on the complete owner set. Once one can observe
// absence, a late owner cannot introduce references to an already removed release.
func (a *Admission) CheckUnused(ctx context.Context, id InstallationID) error {
	if err := contextError(ctx); err != nil {
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
