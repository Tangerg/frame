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
// storage integration, not a callback into plugin code. Held answers for the
// queried installation only, so it returns that installation's releases.
type UseOwner interface {
	ID() string
	Held(context.Context, InstallationID) ([]framevalue.Digest, error)
}

type registeredUseOwner struct {
	id     string
	source UseOwner
}

// RetainedDependency identifies the original business owner that still requires
// an exact release. It is a fresh read projection, not another retention record.
type RetainedDependency struct {
	OwnerID    string
	Dependency ReleaseRef
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
		for _, value := range values {
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
func (a *admissionState) held(ctx context.Context, owner registeredUseOwner, id InstallationID) ([]ReleaseRef, error) {
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
	dependencies := make([]ReleaseRef, 0, len(values))
	for _, release := range values {
		if err := release.Validate(); err != nil {
			return nil, a.untrusted(fmt.Errorf("%w: retained release from %s: %w", frame.ErrInvalid, owner.id, err))
		}
		dependencies = append(dependencies, ReleaseRef{InstallationID: id, Release: release})
	}
	return CompactReleases(dependencies), nil
}

func (a *admissionState) untrusted(failure error) error {
	a.RequireRecovery(failure)
	return errors.Join(ErrRecoveryRequired, failure)
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
	retained, err := a.Retained(ctx, id)
	if err != nil {
		return err
	}
	if len(retained) != 0 {
		return fmt.Errorf("%w: %s", ErrInUse, id)
	}
	return nil
}

func (a Admission) Valid() bool { return a.admissionState != nil }
