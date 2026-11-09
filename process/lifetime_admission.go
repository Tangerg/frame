package process

import (
	"context"
	"errors"

	"github.com/Tangerg/frame"
)

// Only this pre-dispatch boundary proves that a lifetime refusal had no handler
// effects. The same cause returned by a dispatched handler proves nothing.
func lifetimeAdmissionError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case errors.Is(err, frame.ErrCapacity):
		return errors.Join(&Problem{wire.Errors.Capacity, "instance work capacity reached"}, err)
	case errors.Is(err, frame.ErrClosed):
		return errors.Join(&Problem{wire.Errors.Closed, "instance is retiring"}, err)
	default:
		return err
	}
}
