package mcp

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

func TestManagedStreamCancellationPreservesCategoryAndCause(t *testing.T) {
	method, err := portable.NewStreamingMethod[string, int, bool](portable.MethodConfig{Name: "deliver", Kind: portable.Query})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := value.EncodeJSON(1)
	if err != nil {
		t.Fatal(err)
	}
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
					cause := errors.New("managed consumer stopped waiting for the original chunk")
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
					call, abort := context.WithCancelCause(call)
					defer abort(nil)
					called := false
					delivery := managedDelivery{
						call: call, abort: abort, method: method, maxBytes: int64(len(chunk.String())),
						emit: func(ctx context.Context, _ value.JSON) error {
							called = true
							trigger()
							<-ctx.Done()
							return nil
						},
					}
					err := delivery.receive(wait, 1, chunk)
					err = errors.Join(err, delivery.finish())
					if !errors.Is(err, category) || !errors.Is(err, cause) {
						t.Fatalf("managed rejection lost cancellation category or cause: %v", err)
					}
					if called == timing.before || delivery.received != 0 {
						t.Fatalf("canceled chunk called=%t before=%t acknowledged=%d", called, timing.before, delivery.received)
					}
				})
			})
		}
	}
}
