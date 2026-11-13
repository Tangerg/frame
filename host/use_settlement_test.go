package host

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type settlementProgressContext struct {
	context.Context
	advance func()
}

func (ctx settlementProgressContext) Done() <-chan struct{} {
	ctx.advance()
	return ctx.Context.Done()
}

func TestSettlementWaitReadsConclusionBeforeObserverCancellation(t *testing.T) {
	first, second := errors.New("first settlement failure"), errors.New("second settlement failure")
	for _, retryFails := range []bool{false, true} {
		name := "confirmed completion"
		if retryFails {
			name = "new failure"
		}
		t.Run(name, func(t *testing.T) {
			for range 128 {
				settlement := newSettlement()
				settlement.fail(first)
				observed, _ := settlement.requestRetry()
				parent, cancel := context.WithCancel(t.Context())
				cancel()
				ctx := settlementProgressContext{Context: parent, advance: sync.OnceFunc(func() {
					if retryFails {
						settlement.fail(second)
					} else {
						settlement.complete()
					}
				})}
				err := settlement.await(ctx, observed)
				if !retryFails && err != nil || retryFails && !errors.Is(err, second) {
					t.Fatalf("observer cancellation replaced the original conclusion: %v", err)
				}
			}
		})
	}
}
