package frame_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

type opaqueCloseFailure struct{ inspections atomic.Int32 }

func (*opaqueCloseFailure) Error() string { return "opaque original failure" }

func (e *opaqueCloseFailure) Unwrap() []error {
	e.inspections.Add(1)
	return []error{frame.ErrUnconfirmedRelease, errors.New("original failure detail")}
}

func TestResourceStatePreservesOpaqueFailure(t *testing.T) {
	original := &opaqueCloseFailure{}
	state, err := (frame.CloseOutcome{}).ResourceState(original)
	if state != frame.ResourceUnconfirmed || err != original || original.inspections.Load() != 0 {
		t.Fatalf("close projection reinterpreted an opaque failure: state=%s original=%t inspections=%d", state, err == original, original.inspections.Load())
	}
}

func TestCloseConclusionsDoNotCreateOriginalFailures(t *testing.T) {
	for _, released := range []bool{false, true} {
		for _, failures := range []uint64{0, 1} {
			outcome := frame.CloseOutcome{Released: released, OmittedFailures: failures}
			owner := frame.NewLifetime(t.Context())
			causes := owner.ObserveCloseCauses()
			resource, err := owner.Own(frame.Resource{Name: "child conclusion", Close: func(context.Context) (frame.ReleaseState, error) {
				return outcome.ResourceState(outcome.Err())
			}})
			if err != nil {
				t.Fatal(err)
			}
			closing := owner.BeginClose(frame.ErrClosed)
			first, err := closing.Wait(t.Context())
			if first.Released != released || errors.Is(err, frame.ErrCloseFailed) != (failures > 0) || errors.Is(err, frame.ErrUnconfirmedRelease) == released {
				t.Errorf("parent lost original close conclusion: released=%t failed=%t outcome=%+v error=%v", released, failures > 0, first, err)
			}
			if causes.Err() != nil {
				t.Errorf("derived close conclusion became another original failure: %v", causes.Err())
			}
			if !released {
				if err := resource.ConfirmReleased(nil); err != nil {
					t.Fatal(err)
				}
				if outcome, _ := closing.WaitReleased(t.Context()); !outcome.Released {
					t.Errorf("original late proof did not conclude close: %+v", outcome)
				}
			}
		}
	}
}

func TestCloseCompositionPreservesSharedFailureIdentities(t *testing.T) {
	first, second := errors.New("first original failure"), errors.New("second original failure")
	var observations []*frame.CloseCauses
	var failures []error
	for _, original := range []error{first, second} {
		owner := frame.NewLifetime(t.Context())
		causes := owner.ObserveCloseCauses()
		if _, err := owner.Own(frame.Resource{Name: "original execution", Close: func(context.Context) (frame.ReleaseState, error) {
			return frame.ResourceReleased, original
		}}); err != nil {
			t.Fatal(err)
		}
		if outcome, err := owner.Close(t.Context()); !outcome.Released || !errors.Is(err, original) {
			t.Fatalf("original close lost its failure: outcome=%+v error=%v", outcome, err)
		} else {
			failures = append(failures, err)
		}
		observations = append(observations, causes)
	}
	combined := observations[0].Join(failures...)
	combined = observations[0].Join(combined, failures[0])
	for _, original := range []error{first, second} {
		if !errors.Is(combined, original) || strings.Count(combined.Error(), original.Error()) != 1 {
			t.Errorf("composition lost or duplicated an original failure: original=%v combined=%v", original, combined)
		}
	}
	if errors.Is(observations[0].Err(), second) {
		t.Error("composing a projection advanced the original observation")
	}
	owner := frame.NewLifetime(t.Context())
	causes := owner.ObserveCloseCauses()
	if _, err := owner.Own(frame.Resource{Name: "composed close", Close: func(context.Context) (frame.ReleaseState, error) {
		return frame.ResourceReleased, combined
	}}); err != nil {
		t.Fatal(err)
	}
	if outcome, err := owner.Close(t.Context()); !outcome.Released || !errors.Is(err, first) || !errors.Is(err, second) {
		t.Fatalf("parent discarded composed original failures: outcome=%+v error=%v", outcome, err)
	}
	for _, original := range []error{first, second} {
		if strings.Count(causes.Err().Error(), original.Error()) != 1 {
			t.Errorf("parent created another failure identity: %v", causes.Err())
		}
	}
}
