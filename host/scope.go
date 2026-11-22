package host

import (
	"context"
	"fmt"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

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
	if !g.Valid() {
		return ""
	}
	return g.key.id
}

func (g ScopeGrant) Generation() uint64 {
	if !g.Valid() {
		return 0
	}
	return g.number
}

// Valid reports whether the grant names an issued generation; the zero grant does not.
func (g ScopeGrant) Valid() bool { return g.scopeGeneration != nil }

// WireID identifies the scope independently of its generation. A tenant's
// chosen name cannot collide with the installation namespace on a remote Realm.
func (g ScopeGrant) WireID() ScopeID {
	if !g.Valid() {
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
	if !grant.Valid() {
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
