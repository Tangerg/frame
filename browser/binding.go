package browser

import (
	"context"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

// acquirePlanRequest is the canonical identity of an AcquirePlan operation.
type acquirePlanRequest struct {
	Binding string
	Parent  string
}

// AcquirePlan admits a fresh declaration selection. Its preparation reads stay
// attached until the original Host approves the actual execution.
func (b *Bridge) AcquirePlan(ctx context.Context, token, operationID string, plan framehost.BindingPlan, parentUseID string) (grant UseGrant, returned error) {
	binding, err := plan.Snapshot()
	if err != nil {
		return UseGrant{}, err
	}
	session, err := b.liveSession(token)
	if err != nil {
		return UseGrant{}, err
	}
	if !frame.ValidName(operationID) || len(binding.Roots) != 1 || len(binding.UnitRoots) != 0 {
		return UseGrant{}, frame.ErrInvalid
	}
	canonical, err := binding.CanonicalJSON()
	if err != nil {
		return UseGrant{}, err
	}
	digest, err := requestDigest(acquirePlanRequest{Binding: string(canonical), Parent: parentUseID})
	if err != nil {
		return UseGrant{}, err
	}
	identity := controlRequestIdentity{Kind: acquirePlanOperation, Realm: session.realm, Digest: digest}
	operation, replay, err := b.begin(ctx, operationID, identity, func(operation *controlOperation) error {
		if session.cleanupOnly {
			return errRealmSealed
		}
		operation.parentID = parentUseID
		return nil
	})
	if err != nil {
		return UseGrant{}, err
	}
	if replay {
		if err := operation.repeats(identity); err != nil {
			return UseGrant{}, err
		}
		return b.awaitGrant(ctx, operation)
	}
	defer func() { returned = b.finish(ctx, operationID, operation, returned) }()
	if err := b.reserveReceipt(ctx, operationID, operation); err != nil {
		return UseGrant{}, err
	}
	// A child must enter through its parent's exact grant before any new
	// provider can execute. A root Realm bootstraps its own trusted execution.
	if parentUseID == "" {
		if _, err = b.config.Host.ActivatePlan(ctx, RealmID(session.realm), plan); err != nil {
			return UseGrant{}, err
		}
	}
	entry, err := b.planExecution(session, binding, parentUseID, operation)
	if err != nil {
		return UseGrant{}, err
	}
	config, err := b.useConfig(session, entry, parentUseID)
	if err != nil {
		return UseGrant{}, err
	}
	use, err := b.config.Host.Acquire(ctx, plan, config)
	if err != nil {
		return UseGrant{}, err
	}
	return b.deliverUse(ctx, session, entry, use, operationID, operation)
}

// planExecution finds the execution that provides a plan's root: the parent
// grant's execution for a child, otherwise the Realm's one ready execution of
// the root unit and configuration.
func (b *Bridge) planExecution(session *realmSession, binding framehost.BindingSnapshot, parentUseID string, operation *controlOperation) (*browserExecution, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var entry *browserExecution
	if parentUseID != "" {
		parent := b.uses[parentUseID]
		if parent != nil && parent.grant.Realm == session.realm && !parent.released() {
			entry = b.executions.get(parent.grant.Execution.ID)
		}
	} else {
		var err error
		if entry, err = b.readyExecutionLocked(session.realm, binding); err != nil {
			return nil, err
		}
	}
	if entry == nil || entry.isFenced() || session.cleanupOnly {
		return nil, framehost.ErrStaleBinding
	}
	operation.execution = entry.plan.Execution.ID
	return entry, nil
}

func (b *Bridge) readyExecutionLocked(realm Realm, binding framehost.BindingSnapshot) (*browserExecution, error) {
	root := binding.Roots[0].Unit()
	var configuration value.Digest
	for _, unit := range binding.Units {
		if unit.Source == root {
			configuration = unit.Configuration
			break
		}
	}
	var found *browserExecution
	for _, candidate := range b.executions.all() {
		if candidate.isFenced() || candidate.plan.Realm != realm || !candidate.hasReady() || candidate.attempt.Source != root || candidate.plan.Units[0].Instance.Configuration != configuration {
			continue
		}
		if found != nil {
			return nil, framehost.ErrAmbiguous
		}
		found = candidate
	}
	return found, nil
}
