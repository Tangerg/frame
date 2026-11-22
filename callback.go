package frame

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

func CallbackPanicError(callback string, value any) error {
	// Formatting an arbitrary panic value could reenter plugin code or Goexit.
	diagnostic := fmt.Errorf("frame: %s panicked", callback)
	if cause, ok := value.(error); ok {
		return errors.Join(diagnostic, cause)
	}
	return diagnostic
}

func invokeCancel(cancel func(error), cause error) error {
	return InvokeCallback("resource cancellation", func() error {
		cancel(cause)
		return nil
	})
}

func InvokeCallback(name string, callback func() error) error {
	if callback == nil {
		return ErrInvalid
	}
	// Goexit cannot be recovered. Join the original callback's goroutine so
	// its exit cannot abandon the caller's remaining cleanup or settlement.
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = fmt.Errorf("frame: %s exited without returning", name)
		defer func() {
			if value := recover(); value != nil {
				err = CallbackPanicError(name, value)
			}
		}()
		err = callback()
	}()
	<-done
	return err
}

func invokeResourceClose(ctx context.Context, closeResource func(context.Context) (ReleaseState, error)) (state ReleaseState, err error) {
	state = ResourceUnconfirmed
	defer func() {
		if value := recover(); value != nil {
			state = ResourceUnconfirmed
			err = CallbackPanicError("resource close", value)
		}
	}()
	state, err = closeResource(ctx)
	if !slices.Contains(state.JSONSchemaEnum(), string(state)) {
		return ResourceUnconfirmed, errors.Join(err, fmt.Errorf("%w: resource close state", ErrInvalid))
	}
	return state, err
}
