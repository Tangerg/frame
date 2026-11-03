package frame

import (
	"context"
	"fmt"

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

// scopeState is one scope's current generation. The generation is open until
// its context is cancelled; fencing never reopens it.
type scopeState struct {
	context    context.Context
	cancel     context.CancelFunc
	generation uint64
}

func (s scopeState) open() bool { return s.context != nil && s.context.Err() == nil }

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

func (g ScopeGrant) ID() ScopeID        { return g.key.id }
func (g ScopeGrant) Generation() uint64 { return g.generation }

// issued distinguishes a grant from the zero value, which grants nothing.
func (g ScopeGrant) issued() bool { return g.owner != nil }

// WireID identifies the scope independently of its generation. A tenant's
// chosen name cannot collide with the installation namespace on a remote Realm.
func (g ScopeGrant) WireID() ScopeID {
	if !g.issued() {
		return ""
	}
	return ScopeID(g.key.namespace.wirePrefix() + framevalue.Hash([]byte(g.key.id)).String())
}

func (a *Admission) OpenScope(ctx context.Context, id ScopeID) (ScopeGrant, error) {
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
func (a *Admission) checkScopeCapacity(keys ...scopeKey) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	missing := make(map[scopeKey]bool, len(keys))
	for _, key := range keys {
		if _, exists := a.scopes[key]; !exists {
			missing[key] = true
		}
	}
	if len(a.scopes)+len(missing) > a.limits.MaxScopes {
		return fmt.Errorf("%w: scopes", ErrCapacity)
	}
	return nil
}

// openScope grants the open generation, starting a new one after a fence.
func (a *Admission) openScope(key scopeKey) (ScopeGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[key]
	if !exists && len(a.scopes) >= a.limits.MaxScopes {
		return ScopeGrant{}, fmt.Errorf("%w: scopes", ErrCapacity)
	}
	if !state.open() {
		state = a.nextGenerationLocked(key, state)
	}
	return a.grantLocked(key, state), nil
}

// Restoring committed enabled state cannot revoke a later local fence. A cold
// scope has no earlier grant to preserve, so only that case starts a generation.
func (a *Admission) restoreScope(key scopeKey) (ScopeGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[key]
	if !exists {
		if len(a.scopes) >= a.limits.MaxScopes {
			return ScopeGrant{}, fmt.Errorf("%w: scopes", ErrCapacity)
		}
		state = a.nextGenerationLocked(key, state)
	}
	return a.grantLocked(key, state), nil
}

func (a *Admission) nextGenerationLocked(key scopeKey, previous scopeState) scopeState {
	next := scopeState{generation: previous.generation + 1}
	next.context, next.cancel = context.WithCancel(context.Background())
	a.scopes[key] = next
	return next
}

func (a *Admission) grantLocked(key scopeKey, state scopeState) ScopeGrant {
	return ScopeGrant{owner: a, key: key, generation: state.generation, context: state.context}
}

// FenceScope is a local monotonic action and never waits for storage or the
// admission slot. Unknown scopes have no grants to revoke and allocate no
// record. Fencing does not claim remote work has finished.
func (a *Admission) FenceScope(id ScopeID) { a.fenceScope(applicationScope(id)) }

func (a *Admission) fenceScope(key scopeKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if state, exists := a.scopes[key]; exists {
		state.cancel()
	}
}

// Scope observes the original generation and fence even while recovery blocks
// new admission. The observation does not grant authority to use or reopen it.
func (a *Admission) Scope(id ScopeID) (ScopeObservation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, exists := a.scopes[applicationScope(id)]
	if !exists {
		return ScopeObservation{}, false
	}
	return ScopeObservation{ID: id, Generation: state.generation, Fenced: !state.open()}, true
}

func (a *Admission) ValidateScope(grant ScopeGrant) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.scopes[grant.key]
	if grant.owner != a || !state.open() || state.generation != grant.generation {
		return ErrFenced
	}
	return nil
}
