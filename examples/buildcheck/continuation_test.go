package buildcheck

import (
	"context"
	"testing"

	"github.com/Tangerg/frame"
)

func TestAroundPanicCancelsAndJoinsOriginalContinuation(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pass := CompilerPass(func(ctx context.Context, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		defer close(finished)
		return "", context.Cause(ctx)
	})
	wrapper := Around(func(ctx context.Context, source SourceRevision, next Next) (Result, error) {
		go func() { _, _ = next(ctx, source) }()
		<-entered
		panic("failed wrapper")
	})
	defer func() {
		if recover() != "failed wrapper" {
			t.Fatal("lost original panic")
		}
		select {
		case <-finished:
		default:
			t.Fatal("around callback escaped its captured work")
		}
	}()
	_, _ = execute(ctx, SourceRevision{Revision: 1}, []frame.Member[PassMetadata, CompilerPass]{{Value: pass}}, nil, nil, []frame.Member[AroundMetadata, Around]{{Value: wrapper}})
}
