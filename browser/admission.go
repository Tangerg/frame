package browser

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

// An unadmitted execution exits without having run, so its proof is a fixed
// document the Host can produce alone.
const (
	unadmittedExitKind     = "not-admitted"
	unadmittedExitEvidence = `{"admitted":false}`
)

func unadmittedExitProof(attempt sqlite.Attempt) sqlite.ExitProof {
	return attempt.LocalExit(unadmittedExitKind, unadmittedExitEvidence)
}

func isUnadmittedRecord(record sqlite.AttemptRecord) bool {
	return record.ExitProven() && record.Proof != nil && sameWire(*record.Proof, unadmittedExitProof(record.Attempt))
}

// Poll does not transfer an exit obligation. The Runtime reserves its original
// pending owner before this call, so a lost reply cannot orphan admitted work.
func (b *Bridge) AdmitPlan(ctx context.Context, token string, plan LocalPlan) (PlanAdmission, error) {
	if err := b.checkRequest(admitPlanMethod, plan); err != nil {
		return PlanAdmission{}, err
	}
	session, err := b.anySession(token)
	if err != nil {
		return PlanAdmission{}, err
	}
	if plan.Protocol != Protocol || plan.Authority.ID != b.authority.ID || plan.Realm != session.realm {
		return PlanAdmission{}, framehost.ErrStaleBinding
	}
	if err := frame.ContextError(ctx); err != nil {
		return PlanAdmission{}, err
	}
	result := PlanAdmission{Authority: b.authority, Realm: plan.Realm, Execution: plan.Execution, Decision: PlanCleanupRequired}
	if err := b.checkResponse(result); err != nil {
		return PlanAdmission{}, err
	}
	b.mu.Lock()
	entry := b.executions.get(plan.Execution.ID)
	if entry == nil {
		b.mu.Unlock()
		return b.recordedPlanAdmission(ctx, plan, result)
	}
	if !sameWire(entry.plan, plan) {
		b.mu.Unlock()
		return PlanAdmission{}, framehost.ErrStaleBinding
	}
	admitted := entry.runtimeOwnsExit()
	// The pending Runtime owner already exists. A failed currentness check
	// must retain that owner instead of racing a Host non-admission proof.
	cleanup := session.cleanupOnly || !entry.admitRuntime()
	b.mu.Unlock()
	if cleanup {
		return b.admitForCleanup(ctx, entry, admitted, plan, result)
	}
	return b.admitCurrent(ctx, session, entry, plan, result)
}

// admitForCleanup answers a plan that may no longer start. An execution never
// admitted is settled here; otherwise its Runtime owes the exit proof.
func (b *Bridge) admitForCleanup(ctx context.Context, entry *browserExecution, admitted bool, plan LocalPlan, result PlanAdmission) (PlanAdmission, error) {
	if !admitted {
		state, err := b.closeExecution(ctx, entry)
		if err != nil {
			return PlanAdmission{}, err
		}
		if state == frame.ResourceReleased {
			result.Decision = PlanNotAdmitted
			return result, nil
		}
	}
	return b.recordedPlanAdmission(ctx, plan, result)
}

func (b *Bridge) admitCurrent(ctx context.Context, session *realmSession, entry *browserExecution, plan LocalPlan, result PlanAdmission) (PlanAdmission, error) {
	if err := entry.setup.ValidateCurrent(ctx); err != nil {
		b.fence(entry)
		if !errors.Is(err, framehost.ErrStaleBinding) && !errors.Is(err, framehost.ErrFenced) && !errors.Is(err, frame.ErrClosed) {
			return PlanAdmission{}, err
		}
		return b.recordedPlanAdmission(ctx, plan, result)
	}
	b.mu.Lock()
	live := b.executions.get(plan.Execution.ID) == entry && !entry.isFenced() && !session.cleanupOnly
	b.mu.Unlock()
	if !live {
		return b.recordedPlanAdmission(ctx, plan, result)
	}
	result.Decision = PlanAdmitted
	return result, nil
}

func (b *Bridge) recordedPlanAdmission(ctx context.Context, plan LocalPlan, result PlanAdmission) (PlanAdmission, error) {
	record, err := b.config.Store.Attempt(ctx, plan.Execution.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanAdmission{}, framehost.ErrUnavailable
	}
	if err != nil {
		return PlanAdmission{}, err
	}
	if !matchesAttempt(record.Attempt, plan.Authority, plan.Realm, plan.Execution, plan.ScopeID, plan.ScopeGeneration) {
		return PlanAdmission{}, framehost.ErrStaleBinding
	}
	switch {
	case isUnadmittedRecord(record):
		result.Decision = PlanNotAdmitted
	case record.ExitProven():
		result.Decision = PlanSettled
	default:
		result.Decision = PlanCleanupRequired
	}
	return result, nil
}

func matchesAttempt(attempt sqlite.Attempt, authority Authority, realm Realm, execution Execution, scopeID, scopeGeneration string) bool {
	return attempt.Profile == framehost.BrowserJS && attempt.ID == execution.ID &&
		attempt.Realm == string(RealmID(realm)) && authority.Epoch == strconv.FormatUint(attempt.Epoch, 10) &&
		execution.Generation == strconv.FormatUint(attempt.Generation, 10) && scopeID == string(attempt.Scope) &&
		scopeGeneration == strconv.FormatUint(attempt.ScopeGeneration, 10)
}
