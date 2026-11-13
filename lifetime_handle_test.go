package frame_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
)

func TestCloseCopiesObserveOriginalLateCleanup(t *testing.T) {
	ctx := testContext(t)
	owner := frame.NewLifetime(context.Background())
	entered, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	provider, err := owner.Own(frame.Resource{Name: "provider", Close: func(context.Context) (frame.ReleaseState, error) {
		close(entered)
		<-resume
		return frame.ResourceReleased, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := owner.Own(frame.Resource{Name: "consumer", Close: func(context.Context) (frame.ReleaseState, error) {
		return frame.ResourceUnconfirmed, nil
	}}, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release.Do(func() { close(resume) })
		_ = consumer.ConfirmReleased(nil)
		if _, err := owner.Close(testContext(t)); err != nil {
			t.Error(err)
		}
	})
	operation := owner.BeginClose(frame.ErrClosed)
	if _, err := operation.Wait(ctx); !errors.Is(err, frame.ErrUnconfirmedRelease) {
		t.Fatal(err)
	}
	copied := operation
	if err := consumer.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := copied.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("copy ignored the original running cleanup: %v", err)
	}
	release.Do(func() { close(resume) })
	if outcome, err := copied.WaitReleased(ctx); err != nil || !outcome.Released {
		t.Fatalf("copy lost original release: %+v, %v", outcome, err)
	}
}

func TestReplacingCloseHandleCannotResetOriginalRetirement(t *testing.T) {
	ctx := testContext(t)
	owner := frame.NewLifetime(context.Background())
	resource, err := owner.Own(frame.Resource{Name: "late proof", Close: func(context.Context) (frame.ReleaseState, error) {
		return frame.ResourceUnconfirmed, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = resource.ConfirmReleased(nil)
		if _, err := owner.Close(testContext(t)); err != nil {
			t.Error(err)
		}
	})
	operation := owner.BeginClose(frame.ErrClosed)
	if _, err := operation.Wait(ctx); !errors.Is(err, frame.ErrUnconfirmedRelease) {
		t.Fatal(err)
	}
	original := operation
	operation = frame.Close{}
	if _, err := operation.ReleaseProof(); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued handle produced release evidence: %v", err)
	}
	_, proofErr := owner.BeginClose(frame.ErrClosed).ReleaseProof()
	if !errors.Is(proofErr, frame.ErrUnconfirmedRelease) {
		t.Errorf("caller replacement rewrote the original retirement: %v", proofErr)
	}
	if original.Outcome().ID != owner.BeginClose(frame.ErrClosed).Outcome().ID {
		t.Error("caller replacement changed the original close identity")
	}
}

func TestUnissuedCloseHandleCannotObserveOrProveRelease(t *testing.T) {
	var operation frame.Close
	if outcome := operation.Outcome(); outcome.ID != 0 || outcome.Released {
		t.Errorf("unissued handle reported release: %+v", outcome)
	}
	if _, err := operation.Wait(t.Context()); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued handle observed settlement: %v", err)
	}
	if _, err := operation.WaitReleased(t.Context()); !errors.Is(err, frame.ErrInvalid) {
		t.Errorf("unissued handle observed release: %v", err)
	}
}
