package frame_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tangerg/frame"
)

type opaqueCause []string

func (opaqueCause) Error() string { panic("cause must not be formatted") }
func (opaqueCause) Is(error) bool { panic("cause must not be classified") }
func (opaqueCause) As(any) bool   { panic("cause must not be inspected") }
func (opaqueCause) Unwrap() error { panic("cause must not be traversed") }

func TestCancellationPreservesCategoryAndCauseWithoutDiagnostics(t *testing.T) {
	cause := opaqueCause{"original failure"}
	for _, category := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(category.Error(), func(t *testing.T) {
			var ctx context.Context
			if category == context.Canceled {
				owned, cancel := context.WithCancelCause(t.Context())
				cancel(cause)
				ctx = owned
			} else {
				owned, cancel := context.WithDeadlineCause(t.Context(), time.Time{}, cause)
				defer cancel()
				ctx = owned
			}
			err := frame.ContextError(ctx)
			if !errors.Is(err, category) {
				t.Fatal("cancellation category was lost")
			}
			retained, ok := errors.AsType[opaqueCause](err)
			if !ok || len(retained) != len(cause) || &retained[0] != &cause[0] {
				t.Fatal("original cancellation cause was replaced")
			}
		})
	}
	if frame.ContextError(t.Context()) != nil {
		t.Fatal("active context reported cancellation")
	}
	var missing context.Context
	if !errors.Is(frame.ContextError(missing), frame.ErrInvalid) {
		t.Fatal("nil context was admitted")
	}
}

type absentContext struct{ context.Context }

func TestContextErrorRejectsTypedNilBeforeCallingItsMethods(t *testing.T) {
	var missing *absentContext
	if err := frame.ContextError(missing); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("typed nil context admitted: %v", err)
	}
	if _, err := frame.NewLifetimeWithLimits(missing, frame.LifetimeLimits{}); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("typed nil parent admitted: %v", err)
	}
}
