package frame_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/Tangerg/frame"
)

type cleanupCause struct{ payload *retainedPayload }

func (*cleanupCause) Error() string { return "original cleanup failure" }

func TestLocalClosePreservesCauseAndTerminalObservationIsBounded(t *testing.T) {
	owner := frame.NewLifetime(context.Background())
	original := &cleanupCause{}
	if _, err := owner.Own(frame.Resource{Name: "resource", Close: func(context.Context) (frame.ReleaseState, error) { return frame.ResourceReleased, original }}); err != nil {
		t.Fatal(err)
	}
	outcome, err := owner.Close(testContext(t))
	var typed *cleanupCause
	if !outcome.Released || !errors.Is(err, original) || !errors.As(err, &typed) || typed != original || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("local close lost its cause: %+v, %v", outcome, err)
	}
	for _, observe := range []func(context.Context) (frame.CloseOutcome, error){owner.Close, owner.BeginClose(frame.ErrClosed).WaitReleased} {
		observed, err := observe(testContext(t))
		if !observed.Released || !errors.Is(err, frame.ErrCloseFailed) || errors.Is(err, original) || len(observed.Failures) != 1 || observed.Failures[0].Detail != original.Error() {
			t.Fatalf("terminal observation retained or reconstructed a cause: %+v, %v", observed, err)
		}
	}
}

func TestCloseCausesFollowLateProofWithoutAnotherOwner(t *testing.T) {
	owner := frame.NewLifetime(context.Background())
	causes := owner.ObserveCloseCauses()
	if owner.ObserveCloseCauses() != causes {
		t.Fatal("lifetime created a competing cause observation")
	}
	initial, proof := errors.New("initial cleanup failure"), errors.New("late proof diagnostic")
	resource, err := owner.Own(frame.Resource{Name: "original", Close: func(context.Context) (frame.ReleaseState, error) { return frame.ResourceUnconfirmed, initial }})
	if err != nil {
		t.Fatal(err)
	}
	closing := owner.BeginClose(frame.ErrClosed)
	first, err := closing.Wait(testContext(t))
	if first.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) || !errors.Is(causes.Err(), initial) {
		t.Fatalf("missing original failure: %+v, %v", first, err)
	}
	if err := resource.ConfirmReleased(proof); err != nil {
		t.Fatal(err)
	}
	last, err := closing.WaitReleased(testContext(t))
	if !last.Released || last.ID != first.ID || !errors.Is(err, frame.ErrCloseFailed) || !errors.Is(causes.Err(), initial) || !errors.Is(causes.Err(), proof) {
		t.Fatalf("late proof replaced original cause observation: %+v, %v", last, err)
	}
}

func releasedCausePayload(t *testing.T) (*frame.Close, weak.Pointer[retainedPayload]) {
	t.Helper()
	owner := frame.NewLifetime(context.Background())
	causes := owner.ObserveCloseCauses()
	payload := new(retainedPayload)
	original := &cleanupCause{payload: payload}
	if _, err := owner.Own(frame.Resource{Name: "payload", Close: func(context.Context) (frame.ReleaseState, error) { return frame.ResourceReleased, original }}); err != nil {
		t.Fatal(err)
	}
	closing := owner.BeginClose(frame.ErrClosed)
	if outcome, err := closing.WaitReleased(testContext(t)); !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) || !errors.Is(causes.Err(), original) {
		t.Fatalf("close: %+v, %v", outcome, err)
	}
	return closing, weak.Make(payload)
}

func TestTerminalCloseDoesNotRetainDiscardedCauseObservation(t *testing.T) {
	closing, payload := releasedCausePayload(t)
	awaitCollected(t, payload)
	if outcome, err := closing.Wait(testContext(t)); !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("terminal report: %+v, %v", outcome, err)
	}
	runtime.KeepAlive(closing)
}

func TestHostClosePreservesNestedResourceCause(t *testing.T) {
	ctx := testContext(t)
	host, err := frame.NewHost(context.Background(), frame.HostConfig{})
	if err != nil {
		t.Fatal(err)
	}
	original := &cleanupCause{}
	plugin := installNative(t, host, "failure", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo}, Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
		_, err := setup.Own(frame.Resource{Name: "plugin resource", Close: func(context.Context) (frame.ReleaseState, error) { return frame.ResourceReleased, original }})
		return nil, err
	}})
	if _, err := host.ActivateUnit(ctx, "", frame.UnitRef{InstallationID: "failure", Release: plugin.Revision(), UnitID: "main"}); err != nil {
		t.Fatal(err)
	}
	outcome, err := host.Close(ctx)
	if !outcome.Released || !errors.Is(err, original) || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("host dropped nested resource cause: %+v, %v", outcome, err)
	}
}

func TestNestedUseClosePreservesEachOriginalCauseOnce(t *testing.T) {
	ctx := testContext(t)
	host, err := frame.NewHost(context.Background(), frame.HostConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if outcome, err := host.Close(testContext(t)); !outcome.Released {
			t.Errorf("host close: %+v, %v", outcome, err)
		}
	})
	plugin := installNative(t, host, "nested", frame.Unit{Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil }})
	unit := frame.UnitRef{InstallationID: "nested", Release: plugin.Revision(), UnitID: "main"}
	owner := frame.NewLifetime(context.Background())
	t.Cleanup(func() {
		if outcome, err := owner.Close(testContext(t)); !outcome.Released {
			t.Errorf("owner close: %+v, %v", outcome, err)
		}
	})
	scope, err := host.Admission().OpenScope(ctx, "nested-consumer")
	if err != nil {
		t.Fatal(err)
	}
	root, err := host.AcquireUnit(ctx, unit, frame.UseConfig{Owner: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	deepest := root
	for range 8 {
		deepest, err = host.AcquireUnit(ctx, unit, frame.UseConfig{Owner: deepest.Lifetime(), Parent: deepest})
		if err != nil {
			t.Fatal(err)
		}
	}
	original := &cleanupCause{}
	if _, err := deepest.Lifetime().Own(frame.Resource{Name: "deep resource", Close: func(context.Context) (frame.ReleaseState, error) { return frame.ResourceReleased, original }}); err != nil {
		t.Fatal(err)
	}
	outcome, err := root.Close(ctx)
	if !outcome.Released || !errors.Is(err, original) || strings.Count(err.Error(), original.Error()) != 1 {
		t.Fatalf("nested close duplicated or lost original cause: %+v, %v", outcome, err)
	}
}
