package browser

import (
	"context"
	"errors"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

// AcquirePlan admits a fresh declaration selection. Its preparation reads stay
// attached until the original Host approves the actual execution.
func (b *Bridge) AcquirePlan(ctx context.Context, token, operationID string, plan *frame.BindingPlan, parentUseID string) (grant UseGrant, returned error) {
	binding, err := plan.Snapshot()
	if err != nil {
		return UseGrant{}, err
	}

	session, err := b.session(token, true)
	if err != nil {
		return grant, err
	}
	if !frame.ValidName(operationID) || len(binding.Roots) != 1 || len(binding.UnitRoots) != 0 {
		return grant, frame.ErrInvalid
	}
	canonical, err := binding.CanonicalJSON()
	if err != nil {
		return grant, err
	}
	digest, err := requestDigest(struct {
		Binding string
		Parent  string
	}{string(canonical), parentUseID})
	if err != nil {
		return grant, err
	}
	const kind = "acquirePlan"
	b.mu.Lock()
	original, lookupErr := b.operationLocked(ctx, operationID)
	if lookupErr != nil {
		b.mu.Unlock()
		return grant, lookupErr
	}
	if original != nil {
		if original.kind != kind || original.realm != session.realm || original.digest != digest {
			b.mu.Unlock()
			return grant, sqlite.ErrConflict
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return grant, ctx.Err()
		case <-original.done:
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if original.grant == nil {
			return grant, original.failure
		}
		return clone(*original.grant), nil
	}
	if session.cleanupOnly {
		b.mu.Unlock()
		return grant, frame.ErrFenced
	}
	if len(b.operations) >= b.config.MaxPending {
		b.mu.Unlock()
		return grant, frame.ErrCapacity
	}
	operation := &controlOperation{kind: kind, realm: session.realm, digest: digest, parentID: parentUseID, done: make(chan struct{})}
	b.operations[operationID] = operation
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		operation.failure = returned
		b.mu.Unlock()
		returned = errors.Join(returned, b.complete(ctx, operationID, operation))
	}()
	// A child must enter through its parent's exact grant before any new
	// provider can execute. A root Realm bootstraps its own trusted execution.
	if parentUseID == "" {
		if _, err = b.config.Host.ActivatePlan(ctx, RealmID(session.realm), plan); err != nil {
			return grant, err
		}
	}
	var entry *browserExecution
	b.mu.Lock()
	if parentUseID != "" {
		parent := b.uses[parentUseID]
		if parent != nil && parent.grant.Realm == session.realm && parent.releaseID == "" {
			entry = b.executions[parent.grant.Execution.ID]
		}
	} else {
		root := binding.Roots[0].Unit()
		var configuration value.Digest
		for _, unit := range binding.Units {
			if unit.Source == root {
				configuration = unit.Configuration
				break
			}
		}
		for _, candidate := range b.executions {
			if candidate.retiring || candidate.plan.Realm != session.realm || candidate.readyOperation == "" || candidate.attempt.Source != root || candidate.plan.Units[0].Instance.Configuration != configuration {
				continue
			}
			if entry != nil {
				b.mu.Unlock()
				return grant, frame.ErrAmbiguous
			}
			entry = candidate
		}
	}
	if entry == nil || entry.retiring || session.cleanupOnly {
		b.mu.Unlock()
		return grant, frame.ErrStaleBinding
	}
	operation.execution = entry.plan.Execution.ID
	b.mu.Unlock()
	config, err := b.useConfig(session, entry, parentUseID)
	if err != nil {
		return grant, err
	}
	use, err := b.config.Host.Acquire(ctx, plan, config)
	if err != nil {
		return grant, err
	}
	return b.deliverUse(ctx, session, entry, use, operation)
}
