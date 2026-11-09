package browser

import (
	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

// Bridge serializes these transitions under its mutex. Registration cannot
// transfer an exit obligation; only admission creates a Runtime preparation.
type browserExecution struct {
	plan            LocalPlan
	attempt         sqlite.Attempt
	setup           *frame.Setup
	prepared        chan []frame.ExportValue
	registration    *attemptRegistration
	runtime         *runtimePreparation
	sealed          bool
	projectionBytes int64
}

type attemptRegistration struct {
	resource  *frame.ResourceHandle
	uncertain bool
}

type runtimePreparation struct {
	ready *readyReport
}

type readyReport struct {
	operationID string
	digest      value.Digest
}

func (e *browserExecution) recordAttempt(resource *frame.ResourceHandle, failure error) {
	if e.registration != nil || resource == nil {
		panic("browser: invalid attempt registration")
	}
	e.registration = &attemptRegistration{resource: resource, uncertain: failure != nil}
	if failure != nil {
		e.fence()
	}
}

func (e *browserExecution) resourceHandle() *frame.ResourceHandle {
	if e.registration == nil {
		return nil
	}
	return e.registration.resource
}

func (e *browserExecution) approvalUnconfirmed() bool {
	return e.registration != nil && e.registration.uncertain
}

func (e *browserExecution) admitRuntime() bool {
	if e.sealed || e.registration == nil || e.registration.uncertain {
		return false
	}
	if e.runtime == nil {
		e.runtime = &runtimePreparation{}
	}
	return true
}

func (e *browserExecution) runtimeOwnsExit() bool { return e != nil && e.runtime != nil }

func (e *browserExecution) hasReady() bool { return e.runtimeOwnsExit() && e.runtime.ready != nil }

func (e *browserExecution) fence()         { e.sealed = true }
func (e *browserExecution) isFenced() bool { return e.sealed }

func (e *browserExecution) serves(realm Realm, execution Execution) bool {
	return e != nil && !e.sealed && e.registration != nil && !e.registration.uncertain &&
		e.plan.Realm == realm && e.plan.Execution == execution
}

func (e *browserExecution) scopeFence() Fence {
	return Fence{ScopeID: e.plan.ScopeID, ScopeGeneration: e.plan.ScopeGeneration}
}

func (e *browserExecution) replayReady(operationID string, digest value.Digest) (PublicationResult, bool, error) {
	if !e.hasReady() {
		return PublicationResult{}, false, nil
	}
	report := e.runtime.ready
	if report.operationID != operationID || report.digest != digest {
		return PublicationResult{}, true, sqlite.ErrConflict
	}
	return PublicationResult{Decision: Accepted}, true, nil
}

func (e *browserExecution) acceptReady(operationID string, digest value.Digest) (PublicationResult, bool, error) {
	if e.sealed || !e.runtimeOwnsExit() {
		return rejected(problemExecutionReleased), false, nil
	}
	if result, replay, err := e.replayReady(operationID, digest); replay {
		return result, false, err
	}
	e.runtime.ready = &readyReport{operationID: operationID, digest: digest}
	return PublicationResult{Decision: Accepted}, true, nil
}
