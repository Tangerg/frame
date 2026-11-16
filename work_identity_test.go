package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestWorkCopiesShareTheOriginalInterval(t *testing.T) {
	lifetime, err := frame.NewLifetimeWithLimits(t.Context(), frame.LifetimeLimits{MaxWork: 1})
	if err != nil {
		t.Fatal(err)
	}
	work, err := lifetime.Enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer work.Finish()
	copy := work
	copy.Finish()
	next, err := lifetime.Enter(t.Context())
	if err != nil {
		t.Fatalf("finishing the copied handle did not release the original interval: %v", err)
	}
	next.Finish()
	if _, err := work.Cleanup("late", func() error { return nil }); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("original handle accepted a resource after its interval finished: %v", err)
	}
}

func TestFinishedIntervalsCannotReleaseSuccessorWork(t *testing.T) {
	for _, acquisition := range []struct {
		name    string
		acquire func(frame.Lifetime) (frame.Work, error)
	}{
		{"work", func(lifetime frame.Lifetime) (frame.Work, error) { return lifetime.Enter(t.Context()) }},
		{"hold", frame.Lifetime.Hold},
	} {
		t.Run(acquisition.name, func(t *testing.T) {
			ctx := testContext(t)
			lifetime, err := frame.NewLifetimeWithLimits(context.Background(), frame.LifetimeLimits{MaxWork: 1})
			if err != nil {
				t.Fatal(err)
			}
			original, err := acquisition.acquire(lifetime)
			if err != nil {
				t.Fatal(err)
			}
			copied := original
			copied.Finish()
			successor, err := acquisition.acquire(lifetime)
			if err != nil {
				t.Fatal(err)
			}
			defer successor.Finish()
			original.Finish()
			if extra, err := lifetime.Enter(t.Context()); !errors.Is(err, frame.ErrCapacity) || extra.Valid() {
				extra.Finish()
				t.Fatalf("finished interval released its successor's capacity: %v", err)
			}
			if _, err := original.Cleanup("late resource", func() error { return nil }); !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("finished interval accepted a resource: %v", err)
			}
			successor.Finish()
			original.Finish()
			if err := lifetime.AwaitUnheld(ctx); err != nil {
				t.Fatal(err)
			}
			if outcome, err := lifetime.Close(ctx); !outcome.Released || err != nil {
				t.Fatalf("original interval prevented close: %+v, %v", outcome, err)
			}
		})
	}
}

func TestFinishedWorkCannotBeReopenedThroughACopy(t *testing.T) {
	lifetime := frame.NewLifetime(t.Context())
	work, err := lifetime.Enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	copy := work
	work.Finish()
	if _, err := copy.Cleanup("late", func() error { return nil }); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("copied handle accepted a resource after its interval finished: %v", err)
	}
	if _, err := lifetime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
