package frame

import (
	"context"
	"errors"

	"github.com/Tangerg/frame/value"
)

// ContextError keeps cancellation's standard category alongside the owner's
// cause without invoking plugin-controlled diagnostic methods.
func ContextError(ctx context.Context) error {
	if value.IsNil(ctx) {
		return ErrInvalid
	}
	err := ctx.Err()
	if err == nil {
		return nil
	}
	// Distinct dynamic types compare unequal without inspecting the cause.
	if cause := context.Cause(ctx); cause != err {
		return errors.Join(err, cause)
	}
	return err
}
