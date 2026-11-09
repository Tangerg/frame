package cancellation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tangerg/frame/internal/cancellation"
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
			err := cancellation.Err(ctx)
			if !errors.Is(err, category) {
				t.Fatal("cancellation category was lost")
			}
			retained, ok := errors.AsType[opaqueCause](err)
			if !ok || len(retained) != len(cause) || &retained[0] != &cause[0] {
				t.Fatal("original cancellation cause was replaced")
			}
		})
	}
	if cancellation.Err(t.Context()) != nil {
		t.Fatal("active context reported cancellation")
	}
}
