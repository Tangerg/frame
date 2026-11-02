package browser

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
)

const unadmittedExitKind = "not-admitted"

func unadmittedExitProof(attempt sqlite.Attempt) sqlite.ExitProof {
	return sqlite.ExitProof{
		AttemptID: attempt.ID,
		Realm:     attempt.Realm,
		ExitID:    attempt.ID + ":" + unadmittedExitKind,
		Kind:      unadmittedExitKind,
		Evidence:  []byte(`{"admitted":false}`),
	}
}

func isUnadmittedRecord(record sqlite.AttemptRecord) bool {
	return record.Released && record.Proof != nil && equal(*record.Proof, unadmittedExitProof(record.Attempt))
}

// Poll does not transfer an exit obligation. The Runtime reserves its original
// pending owner before this call, so a lost reply cannot orphan admitted work.
func (b *Bridge) AdmitPlan(ctx context.Context, token string, plan LocalPlan) (PlanAdmission, error) {
	if err := b.checkRequest(admitPlanMethod, plan); err != nil {
		return PlanAdmission{}, err
	}
	session, err := b.session(token, false)
	if err != nil {
		return PlanAdmission{}, err
	}
	if plan.Protocol != Protocol || plan.Authority.ID != b.authority.ID || plan.Realm != session.realm {
		return PlanAdmission{}, frame.ErrStaleBinding
	}
	if err := ctx.Err(); err != nil {
		return PlanAdmission{}, err
	}
	result := PlanAdmission{Authority: b.authority, Realm: plan.Realm, Execution: plan.Execution, Decision: PlanCleanupRequired}
	if err := b.checkResponse(result); err != nil {
		return PlanAdmission{}, err
	}
	b.mu.Lock()
	entry := b.executions[plan.Execution.ID]
	if entry == nil {
		b.mu.Unlock()
		return b.recordedPlanAdmission(ctx, plan, result)
	}
	if !equal(entry.plan, plan) {
		b.mu.Unlock()
		return PlanAdmission{}, frame.ErrStaleBinding
	}
	cleanup := entry.retiring || session.cleanupOnly
	admitted := entry.admitted
	if !cleanup {
		// The pending Runtime owner already exists. A failed currentness check
		// must retain that owner instead of racing a Host non-admission proof.
		entry.admitted = true
	}
	b.mu.Unlock()
	if cleanup {
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
	if err := entry.setup.ValidateCurrent(ctx); err != nil {
		b.fence(entry)
		if !errors.Is(err, frame.ErrStaleBinding) && !errors.Is(err, frame.ErrFenced) && !errors.Is(err, frame.ErrClosed) {
			return PlanAdmission{}, err
		}
		return b.recordedPlanAdmission(ctx, plan, result)
	}
	b.mu.Lock()
	live := b.executions[plan.Execution.ID] == entry && !entry.retiring && !session.cleanupOnly
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
		return PlanAdmission{}, frame.ErrUnavailable
	}
	if err != nil {
		return PlanAdmission{}, err
	}
	if !matchesAttempt(record.Attempt, plan.Authority, plan.Realm, plan.Execution, plan.ScopeID, plan.ScopeGeneration) {
		return PlanAdmission{}, frame.ErrStaleBinding
	}
	switch {
	case isUnadmittedRecord(record):
		result.Decision = PlanNotAdmitted
	case record.Released:
		result.Decision = PlanSettled
	default:
		result.Decision = PlanCleanupRequired
	}
	return result, nil
}

func matchesAttempt(attempt sqlite.Attempt, authority Authority, realm Realm, execution Execution, scopeID, scopeGeneration string) bool {
	return attempt.Profile == frame.BrowserJS && attempt.ID == execution.ID &&
		attempt.Realm == string(RealmID(realm)) && authority.Epoch == strconv.FormatUint(attempt.Epoch, 10) &&
		execution.Generation == strconv.FormatUint(attempt.Generation, 10) && scopeID == string(attempt.Scope) &&
		scopeGeneration == strconv.FormatUint(attempt.ScopeGeneration, 10)
}
