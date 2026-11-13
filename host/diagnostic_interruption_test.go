package host_test

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/Tangerg/frame"
)

type interruptedDiagnosticError struct{}

func (*interruptedDiagnosticError) Error() string {
	runtime.Goexit()
	return "unreachable"
}

func TestCleanupDiagnosticGoexitPreservesOriginalConclusion(t *testing.T) {
	for _, state := range []frame.ReleaseState{frame.ResourceReleased, frame.ResourceUnconfirmed} {
		t.Run(string(state), func(t *testing.T) {
			lifetime := frame.NewLifetime(context.Background())
			causes := lifetime.ObserveCloseCauses()
			original := &interruptedDiagnosticError{}
			resource, err := lifetime.Own(frame.Resource{
				Name: "original cleanup",
				Close: func(context.Context) (frame.ReleaseState, error) {
					return state, original
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			closing := lifetime.BeginClose(frame.ErrClosed)
			outcome, err := closing.Wait(testContext(t))
			if outcome.Released != (state == frame.ResourceReleased) || !errors.Is(err, frame.ErrCloseFailed) {
				t.Fatalf("diagnostic formatting abandoned the cleanup conclusion: %+v, %v", outcome, err)
			}
			if !errors.Is(causes.Err(), original) || len(outcome.Failures) != 1 || outcome.Failures[0].Detail == "" {
				t.Fatalf("diagnostic interruption lost the original error or its bounded report: %+v", outcome)
			}
			if state == frame.ResourceUnconfirmed {
				if _, err := closing.ReleaseProof(); !errors.Is(err, frame.ErrUnconfirmedRelease) {
					t.Fatalf("diagnostic interruption invented release evidence: %v", err)
				}
				if err := resource.ConfirmReleased(nil); err != nil {
					t.Fatal(err)
				}
				last, err := closing.WaitReleased(testContext(t))
				if !last.Released || last.ID != outcome.ID || !errors.Is(err, frame.ErrCloseFailed) {
					t.Fatalf("original release proof did not finish the same close: %+v, %v", last, err)
				}
			}
		})
	}
}

func TestReleaseProofDiagnosticGoexitPreservesOriginalProof(t *testing.T) {
	lifetime := frame.NewLifetime(context.Background())
	causes := lifetime.ObserveCloseCauses()
	resource, err := lifetime.Own(frame.Resource{
		Name: "original resource",
		Close: func(context.Context) (frame.ReleaseState, error) {
			return frame.ResourceUnconfirmed, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	closing := lifetime.BeginClose(frame.ErrClosed)
	first, err := closing.Wait(testContext(t))
	if first.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) {
		t.Fatalf("cleanup did not await original release evidence: %+v, %v", first, err)
	}
	original := &interruptedDiagnosticError{}
	returned := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		returned <- resource.ConfirmReleased(original)
	}()
	<-exited
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("diagnostic formatting terminated the original proof caller")
	}
	last, err := closing.WaitReleased(testContext(t))
	if !last.Released || last.ID != first.ID || !errors.Is(err, frame.ErrCloseFailed) || !errors.Is(causes.Err(), original) {
		t.Fatalf("diagnostic interruption lost original release evidence or its cause: %+v, %v", last, err)
	}
}

func TestCleanupPanicDiagnosticGoexitRetainsOriginalCause(t *testing.T) {
	lifetime := frame.NewLifetime(context.Background())
	causes := lifetime.ObserveCloseCauses()
	original := &interruptedDiagnosticError{}
	resource, err := lifetime.Own(frame.Resource{
		Name: "original cleanup",
		Close: func(context.Context) (frame.ReleaseState, error) {
			panic(original)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	closing := lifetime.BeginClose(frame.ErrClosed)
	outcome, err := closing.Wait(testContext(t))
	if outcome.Released || !errors.Is(err, frame.ErrCloseFailed) || !errors.Is(causes.Err(), original) {
		t.Fatalf("panic diagnostic interrupted cleanup or discarded its original cause: %+v, %v", outcome, err)
	}
	if err := resource.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	last, err := closing.WaitReleased(testContext(t))
	if !last.Released || last.ID != outcome.ID || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("original release proof did not conclude interrupted panic cleanup: %+v, %v", last, err)
	}
}
