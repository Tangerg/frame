package process

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestStreamCancellationPreservesCategoryAndCause(t *testing.T) {
	for _, category := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, timing := range []struct {
			name   string
			before bool
			wait   bool
		}{
			{name: "before-call", before: true},
			{name: "before-wait", before: true, wait: true},
			{name: "inside-consumer"},
			{name: "inside-wait", wait: true},
		} {
			t.Run(category.Error()+"/"+timing.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					cause := errors.New("consumer stopped waiting for the original chunk")
					var selected context.Context
					var trigger, cleanup func()
					if category == context.Canceled {
						var cancel context.CancelCauseFunc
						selected, cancel = context.WithCancelCause(t.Context())
						trigger = func() { cancel(cause) }
						cleanup = func() { cancel(nil) }
					} else {
						duration := time.Second
						if timing.before {
							duration = 0
						}
						selected, cleanup = context.WithTimeoutCause(t.Context(), duration, cause)
						trigger = func() {}
					}
					defer cleanup()
					if timing.before {
						trigger()
					}
					call, wait := t.Context(), t.Context()
					if timing.wait {
						wait = selected
					} else {
						call = selected
					}
					stream := newStreamCall(call)
					called := false
					err := stream.deliver(wait, func(ctx context.Context) error {
						called = true
						trigger()
						<-ctx.Done()
						return nil
					})
					err = stream.finish(err)
					if !errors.Is(err, category) || !errors.Is(err, cause) || !errors.Is(err, ErrOutcomeUnknown) {
						t.Fatalf("chunk rejection lost outcome, cancellation category, or cause: %v", err)
					}
					if called == timing.before {
						t.Fatalf("consumer called=%t before=%t", called, timing.before)
					}
				})
			})
		}
	}
}
