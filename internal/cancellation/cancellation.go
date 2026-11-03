// Package cancellation reports why a context ended. The error matches both the
// standard category, context.Canceled or context.DeadlineExceeded, and the
// owner's cause, so a caller can branch on either.
package cancellation

import (
	"context"
	"errors"
)

// Err returns nil while ctx is live.
func Err(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, err) {
		return cause
	}
	return errors.Join(err, cause)
}
