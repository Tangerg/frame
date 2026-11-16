package browser

import (
	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

// Bridge serializes these transitions under its mutex. Registration cannot
// transfer an exit obligation; only admission creates a Runtime preparation.
type browserExecution struct {
	plan            LocalPlan
	attempt         sqlite.Attempt
	setup           framehost.Setup
	prepared        chan []framehost.ExportValue
	resource        frame.ResourceHandle
	approvalCommit  framehost.CommitState
	approvalDone    chan struct{}
	runtime         *runtimePreparation
	fenced          bool
	projectionBytes int64
}

type runtimePreparation struct {
	ready *readyReport
}

type readyReport struct {
	operationID string
	digest      value.Digest
}

func (e *browserExecution) reserveExit(resource frame.ResourceHandle) {
	if e.resource.Valid() || !resource.Valid() {
		panic("browser: invalid attempt registration")
	}
	e.resource = resource
	e.approvalCommit = framehost.CommitIndeterminate
}

func (e *browserExecution) recordApproval(state framehost.CommitState) {
	if !e.resource.Valid() || e.approvalCommit != framehost.CommitIndeterminate {
		panic("browser: invalid approval conclusion")
	}
	switch state {
	case framehost.Committed, framehost.NotCommitted:
		e.approvalCommit = state
	}
}

func (e *browserExecution) approved() bool {
	return e.resource.Valid() && e.approvalCommit == framehost.Committed
}

func (e *browserExecution) admitRuntime() bool {
	if e.fenced || !e.approved() {
		return false
	}
	if e.runtime == nil {
		e.runtime = &runtimePreparation{}
	}
	return true
}

func (e *browserExecution) runtimeOwnsExit() bool { return e != nil && e.runtime != nil }

func (e *browserExecution) hasReady() bool { return e.runtimeOwnsExit() && e.runtime.ready != nil }

func (e *browserExecution) fence()         { e.fenced = true }
func (e *browserExecution) isFenced() bool { return e.fenced }

func (e *browserExecution) serves(realm Realm, execution Execution) bool {
	return e != nil && !e.fenced && e.approved() &&
		e.plan.Realm == realm && e.plan.Execution == execution
}

func (e *browserExecution) replayReady(operationID string, digest value.Digest) (DecisionResult, bool, error) {
	if !e.hasReady() {
		return DecisionResult{}, false, nil
	}
	report := e.runtime.ready
	if report.operationID != operationID || report.digest != digest {
		return DecisionResult{}, true, sqlite.ErrConflict
	}
	return DecisionResult{Decision: Accepted}, true, nil
}

func (e *browserExecution) acceptReady(operationID string, digest value.Digest) (DecisionResult, bool, error) {
	if result, replay, err := e.replayReady(operationID, digest); replay {
		return result, false, err
	}
	if e.fenced || !e.runtimeOwnsExit() {
		return rejectedDecision(problemExecutionReleased), false, nil
	}
	e.runtime.ready = &readyReport{operationID: operationID, digest: digest}
	return DecisionResult{Decision: Accepted}, true, nil
}
