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
	copy := *work
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

func TestFinishedWorkCannotBeReopenedThroughACopy(t *testing.T) {
	lifetime := frame.NewLifetime(t.Context())
	work, err := lifetime.Enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	copy := *work
	work.Finish()
	if _, err := copy.Cleanup("late", func() error { return nil }); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("copied handle accepted a resource after its interval finished: %v", err)
	}
	if _, err := lifetime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
