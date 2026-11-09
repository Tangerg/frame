// Package cancellation reports why a context ended. The error matches both the
// standard category, context.Canceled or context.DeadlineExceeded, and the
// owner's cause, so a caller can branch on either.
package cancellation

import (
	"context"
	"errors"
)

func Err(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	// Cancellation must not execute arbitrary diagnostic methods on its cause.
	return errors.Join(err, context.Cause(ctx))
}
