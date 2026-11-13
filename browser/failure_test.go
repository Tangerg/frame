package browser

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

func TestFailureVocabularyPreservesReceiptAndHTTPMeaning(t *testing.T) {
	for _, test := range []struct {
		cause error
		code  string
	}{
		{framehost.ErrStaleBinding, "stale_binding"},
		{frame.ErrContractMismatch, "contract_mismatch"},
		{frame.ErrUnconfirmedRelease, "close_unconfirmed"},
	} {
		t.Run(test.code, func(t *testing.T) {
			captured := captureControlFailure(test.cause)
			codes := captureReceiptFailures(captured)
			if !reflect.DeepEqual(codes, []string{test.code}) {
				t.Fatalf("receipt classification = %v", codes)
			}
			restored, err := restoreControlFailure(codes)
			if err != nil || !errors.Is(restored, test.cause) {
				t.Fatalf("receipt lost its original category: %v", err)
			}
			problem := classifyProblem(restored)
			if problem.code != test.code || problem.status != http.StatusConflict {
				t.Fatalf("HTTP changed the receipt category: %+v", problem)
			}
		})
	}
}

func TestReceiptPreservesCategoriesHiddenByHTTPProjection(t *testing.T) {
	causes := []error{
		frame.ErrInvalid, frame.ErrClosed, framehost.ErrStaleBinding, framehost.ErrUnavailable,
		framehost.ErrInUse, framehost.ErrAmbiguous, framehost.ErrDependencyCycle, frame.ErrContractMismatch,
		framehost.ErrRecoveryRequired, framehost.ErrFenced, frame.ErrCapacity, framehost.ErrResyncRequired,
		frame.ErrUnconfirmedRelease, sqlite.ErrConflict, context.Canceled, context.DeadlineExceeded,
		sqlite.ErrIndeterminate,
	}
	captured := captureControlFailure(errors.Join(causes...))
	codes := captureReceiptFailures(captured)
	if len(codes) != len(causes) || classifyProblem(captured) != uncertainProblem {
		t.Fatalf("HTTP projection discarded the full receipt: %v", codes)
	}
	restored, err := restoreControlFailure(codes)
	if err != nil {
		t.Fatal(err)
	}
	for _, cause := range causes {
		if !errors.Is(restored, cause) {
			t.Fatalf("receipt lost %v", cause)
		}
	}
	if classifyProblem(captureControlFailure(errors.Join(frame.ErrInvalid, sqlite.ErrConflict))) != invalidProblem {
		t.Fatal("invalid request and identity conflict lost their shared HTTP projection")
	}
}

type controlDiagnostic struct {
	method    string
	interrupt func()
	calls     int
}

func (e *controlDiagnostic) check(method string) {
	if e.method == method {
		e.calls++
		e.interrupt()
	}
}

func (e *controlDiagnostic) Error() string {
	e.check("Error")
	return "private diagnostic"
}

func (e *controlDiagnostic) Is(error) bool {
	e.check("Is")
	return false
}

func (e *controlDiagnostic) As(any) bool {
	e.check("As")
	return false
}

func (e *controlDiagnostic) Unwrap() error {
	e.check("Unwrap")
	return nil
}

func TestControlDiagnosticInterruptionCannotInterruptItsCaller(t *testing.T) {
	for _, interruption := range []struct {
		name string
		run  func()
	}{
		{"panic", func() { panic("diagnostic interrupted") }},
		{"goexit", runtime.Goexit},
	} {
		for _, method := range []string{"Error", "Is", "As", "Unwrap"} {
			t.Run(interruption.name+"/"+method, func(t *testing.T) {
				cause := &controlDiagnostic{method: method, interrupt: interruption.run}
				captured := captureControlFailure(cause)
				calls := cause.calls
				if classifyProblem(captured) != uncertainProblem || !reflect.DeepEqual(captureReceiptFailures(captured), []string{unknownReceiptFailure}) {
					t.Fatal("interrupted diagnostic became a known refusal")
				}
				if errors.Unwrap(captured) != cause || cause.calls != calls {
					t.Fatal("projection replaced or inspected the original cause")
				}
				if (method == "Is" || method == "Unwrap") && calls != 1 {
					t.Fatal("diagnostic interruption was not exercised")
				}
			})
		}
	}
}

type mutableControlDiagnostic struct {
	calls     int
	interrupt func()
}

func (*mutableControlDiagnostic) Error() string { panic("diagnostic must not be formatted") }
func (e *mutableControlDiagnostic) Is(target error) bool {
	e.calls++
	if e.interrupt != nil {
		e.interrupt()
	}
	return target == framehost.ErrStaleBinding
}

func TestJoinedFailureReusesCapturedCategories(t *testing.T) {
	cause := &mutableControlDiagnostic{}
	captured := captureControlFailure(cause)
	calls := cause.calls
	cause.interrupt = runtime.Goexit
	for _, secondary := range []error{
		frame.ErrCapacity,
		&controlDiagnostic{method: "Is", interrupt: runtime.Goexit},
	} {
		joined := captureControlFailure(errors.Join(captured, secondary))
		if !slices.Contains(captureReceiptFailures(joined), staleBindingFailure) || cause.calls != calls {
			t.Fatal("secondary failure reclassified or discarded the original failure")
		}
	}
	if problem := classifyProblem(errors.Join(captured, sqlite.ErrIndeterminate)); problem != uncertainProblem || cause.calls != calls {
		t.Fatal("uncertain settlement did not dominate the captured refusal")
	}
}

func TestReceiptFailureAdmissionRejectsAliasesAndDuplicateCategories(t *testing.T) {
	for _, codes := range [][]string{
		nil, {}, {"stale"}, {"contract"}, {"unconfirmed"}, {"recovery"}, {"resync"}, {"unknown"},
		{"future_category"}, {staleBindingFailure, staleBindingFailure},
	} {
		if _, err := restoreControlFailure(codes); !errors.Is(err, sqlite.ErrConflict) {
			t.Fatalf("unadmitted receipt categories were accepted: %v", codes)
		}
	}
	codes := []string{contractMismatchFailure, staleBindingFailure}
	restored, err := restoreControlFailure(codes)
	if err != nil {
		t.Fatal(err)
	}
	codes[0] = "changed"
	projected := captureReceiptFailures(restored)
	projected[0] = "changed"
	if !errors.Is(restored, frame.ErrContractMismatch) || slices.Contains(captureReceiptFailures(restored), "changed") {
		t.Fatal("caller mutation changed an admitted failure")
	}
	if captureControlFailure(nil) != nil || captureReceiptFailures(nil) != nil {
		t.Fatal("success acquired a failure classification")
	}
}

type blockingControlDiagnostic struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*blockingControlDiagnostic) Error() string { panic("diagnostic must not be formatted") }
func (e *blockingControlDiagnostic) Is(error) bool {
	e.once.Do(func() { close(e.entered) })
	<-e.release
	return false
}

func TestControlFailureCaptureJoinsItsDiagnosticWork(t *testing.T) {
	cause := &blockingControlDiagnostic{entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(cause.release) })
	t.Cleanup(release)
	completed := make(chan error, 1)
	go func() { completed <- captureControlFailure(cause) }()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	select {
	case <-cause.entered:
	case <-ctx.Done():
		t.Fatal("diagnostic capture did not start")
	}
	select {
	case <-completed:
		t.Fatal("capture detached its unfinished diagnostic")
	default:
	}
	release()
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal("capture did not join its diagnostic")
	}
}

type opaqueControlFailure []string

func (opaqueControlFailure) Error() string { panic("diagnostic must not be formatted") }

func TestControlFailureDoesNotCompareUncomparableErrors(t *testing.T) {
	cause := opaqueControlFailure{"private diagnostic"}
	if classifyProblem(cause) != uncertainProblem {
		t.Fatal("unclassified opaque cause became a known refusal")
	}
}
