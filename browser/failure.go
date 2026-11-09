package browser

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
)

const (
	invalidFailure          = "invalid"
	closedFailure           = "closed"
	staleBindingFailure     = "stale_binding"
	unavailableFailure      = "unavailable"
	inUseFailure            = "in_use"
	ambiguousFailure        = "ambiguous"
	dependencyCycleFailure  = "dependency_cycle"
	contractMismatchFailure = "contract_mismatch"
	recoveryRequiredFailure = "recovery_required"
	fencedFailure           = "fenced"
	capacityFailure         = "capacity"
	resyncRequiredFailure   = "resync_required"
	closeUnconfirmedFailure = "close_unconfirmed"
	conflictFailure         = "conflict"
	cancelledFailure        = "cancelled"
	deadlineFailure         = "deadline"
	unknownReceiptFailure   = "outcome_unknown"
)

type httpProblemKind struct {
	code, message string
	status        int
}

var (
	deniedProblem      = httpProblemKind{"permission_denied", "the original Realm is not authorized for this operation", http.StatusForbidden}
	invalidProblem     = httpProblemKind{"invalid_plan", "the control request does not match its original identity", http.StatusBadRequest}
	capacityProblem    = httpProblemKind{capacityFailure, "the configured capacity is exhausted", http.StatusTooManyRequests}
	uncertainProblem   = httpProblemKind{unknownReceiptFailure, "observe the original operation to resolve its outcome", http.StatusServiceUnavailable}
	unavailableProblem = httpProblemKind{unavailableFailure, "the original control operation is unavailable", http.StatusServiceUnavailable}
)

type failureCategory struct {
	code    string
	err     error
	problem httpProblemKind
}

// Receipts preserve every category; HTTP uses the first matching projection.
// Unknown outcome must dominate a secondary refusal after dispatch.
var receiptFailures = []failureCategory{
	{unknownReceiptFailure, sqlite.ErrIndeterminate, uncertainProblem},
	{fencedFailure, frame.ErrFenced, deniedProblem},
	{invalidFailure, frame.ErrInvalid, invalidProblem},
	{conflictFailure, sqlite.ErrConflict, invalidProblem},
	{capacityFailure, frame.ErrCapacity, capacityProblem},
	{contractMismatchFailure, frame.ErrContractMismatch, httpProblemKind{contractMismatchFailure, "the exact contract does not match", http.StatusConflict}},
	{staleBindingFailure, frame.ErrStaleBinding, httpProblemKind{staleBindingFailure, "the original binding is no longer current", http.StatusConflict}},
	{closedFailure, frame.ErrClosed, httpProblemKind{closedFailure, "the original owner is closed", http.StatusConflict}},
	{closeUnconfirmedFailure, frame.ErrUnconfirmedRelease, httpProblemKind{closeUnconfirmedFailure, "the original exit has not yet been confirmed", http.StatusConflict}},
	{unavailableFailure, frame.ErrUnavailable, unavailableProblem},
	{inUseFailure, frame.ErrInUse, unavailableProblem},
	{ambiguousFailure, frame.ErrAmbiguous, unavailableProblem},
	{dependencyCycleFailure, frame.ErrDependencyCycle, unavailableProblem},
	{recoveryRequiredFailure, frame.ErrRecoveryRequired, unavailableProblem},
	{resyncRequiredFailure, frame.ErrResyncRequired, unavailableProblem},
	{cancelledFailure, context.Canceled, unavailableProblem},
	{deadlineFailure, context.DeadlineExceeded, unavailableProblem},
}

var controlProblems = func() []httpProblemKind {
	var problems []httpProblemKind
	for _, category := range receiptFailures {
		if !slices.ContainsFunc(problems, func(p httpProblemKind) bool { return p.code == category.problem.code }) {
			problems = append(problems, category.problem)
		}
	}
	return problems
}()

// Only admission writes these categories. The original cause remains available
// for explicit inspection but cannot change receipt or HTTP classification.
type controlFailure struct {
	codes []string
	cause error
}

func (f *controlFailure) Error() string {
	return "browser: control operation failed: " + strings.Join(f.codes, ", ")
}

func (f *controlFailure) Unwrap() error { return f.cause }

func (f *controlFailure) Is(target error) bool {
	if target == nil || !reflect.ValueOf(target).Comparable() {
		return false
	}
	for _, category := range receiptFailures {
		if target == category.err {
			return slices.Contains(f.codes, category.code)
		}
	}
	return false
}

func unknownControlFailure() *controlFailure {
	return &controlFailure{codes: []string{unknownReceiptFailure}, cause: sqlite.ErrIndeterminate}
}

func captureControlFailure(err error) error {
	if err == nil {
		return nil
	}
	if captured, ok := err.(*controlFailure); ok && captured != nil {
		return captured
	}
	failure := unknownControlFailure()
	failure.cause = err
	done := make(chan struct{})
	// Goexit cannot be recovered by its caller. This one diagnostic child is
	// always joined: cancellation never detaches it or repeats business work.
	go func() {
		defer func() {
			_ = recover()
			close(done)
		}()
		failure.capture()
	}()
	<-done
	return failure
}

func (f *controlFailure) capture() {
	matched := make([]bool, len(receiptFailures))
	pending := []error{f.cause}
	// A captured owner ends its branch; errors.Is would continue into its raw
	// cause after a negative match and execute previously captured diagnostics.
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		if captured, ok := current.(*controlFailure); ok && captured != nil {
			for index, category := range receiptFailures {
				if slices.Contains(captured.codes, category.code) {
					matched[index] = true
					if !slices.Contains(f.codes, category.code) {
						f.codes = append(f.codes, category.code)
					}
				}
			}
			continue
		}
		comparable := reflect.ValueOf(current).Comparable()
		matcher, hasMatcher := current.(interface{ Is(error) bool })
		for index, category := range receiptFailures {
			if matched[index] {
				continue
			}
			if comparable && current == category.err || hasMatcher && matcher.Is(category.err) {
				matched[index] = true
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			for index := len(children) - 1; index >= 0; index-- {
				pending = append(pending, children[index])
			}
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	if codes := selectedFailureCodes(matched); len(codes) > 0 {
		f.codes = codes
	}
}

func captureReceiptFailures(err error) []string {
	if err == nil {
		return nil
	}
	return slices.Clone(captureControlFailure(err).(*controlFailure).codes)
}

func restoreControlFailure(codes []string) (*controlFailure, error) {
	if len(codes) == 0 || len(codes) > len(receiptFailures) {
		return nil, sqlite.ErrConflict
	}
	matched := make([]bool, len(receiptFailures))
	var causes []error
	for _, code := range codes {
		index := slices.IndexFunc(receiptFailures, func(category failureCategory) bool { return category.code == code })
		if index < 0 || matched[index] {
			return nil, sqlite.ErrConflict
		}
		matched[index] = true
		causes = append(causes, receiptFailures[index].err)
	}
	return &controlFailure{codes: selectedFailureCodes(matched), cause: errors.Join(causes...)}, nil
}

func selectedFailureCodes(matched []bool) []string {
	var codes []string
	for index, category := range receiptFailures {
		if matched[index] {
			codes = append(codes, category.code)
		}
	}
	return codes
}

func classifyProblem(err error) httpProblemKind {
	codes := captureReceiptFailures(err)
	for _, category := range receiptFailures {
		if slices.Contains(codes, category.code) {
			return category.problem
		}
	}
	return unavailableProblem
}
