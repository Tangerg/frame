package sqlite

import (
	"context"
	"errors"
)

// cancellationError retains both the standard cancellation category and the owner's cause.
func cancellationError(ctx context.Context) error {
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
