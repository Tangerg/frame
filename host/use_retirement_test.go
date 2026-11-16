package host

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
)

type retirementProgressContext struct {
	context.Context
	advance func()
}

func (ctx retirementProgressContext) Done() <-chan struct{} {
	ctx.advance()
	return ctx.Context.Done()
}

func TestRetirementWaitReadsConclusionBeforeObserverCancellation(t *testing.T) {
	first, second := errors.New("first settlement failure"), errors.New("second settlement failure")
	for _, retryFails := range []bool{false, true} {
		name := "confirmed completion"
		if retryFails {
			name = "new failure"
		}
		t.Run(name, func(t *testing.T) {
			for range 128 {
				retirement := newUseRetirement(frame.NewLifetime(t.Context()))
				retirement.fail(first)
				observed, _ := retirement.requestRetry()
				parent, cancel := context.WithCancel(t.Context())
				cancel()
				ctx := retirementProgressContext{Context: parent, advance: sync.OnceFunc(func() {
					if retryFails {
						retirement.fail(second)
					} else {
						retirement.complete()
					}
				})}
				err := retirement.await(ctx, observed)
				if !retryFails && err != nil || retryFails && !errors.Is(err, second) {
					t.Fatalf("observer cancellation replaced the original conclusion: %v", err)
				}
			}
		})
	}
}
