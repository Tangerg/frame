package frame_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

func TestLateProofRetainsDependenciesUntilRunningCleanupJoins(t *testing.T) {
	ctx := testContext(t)
	owner := frame.NewLifetime(context.Background())
	providerClosed := make(chan struct{})
	var cleanupJoined atomic.Bool
	provider, err := owner.Own(frame.Resource{Name: "provider", Close: func(context.Context) (frame.ReleaseState, error) {
		if !cleanupJoined.Load() {
			t.Error("provider released before its consumer cleanup returned")
		}
		close(providerClosed)
		return frame.ResourceReleased, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	consumer, err := owner.Own(frame.Resource{Name: "consumer", Close: func(context.Context) (frame.ReleaseState, error) {
		close(entered)
		<-finish
		cleanupJoined.Store(true)
		return frame.ResourceUnconfirmed, nil
	}}, provider, provider)
	if err != nil {
		t.Fatal(err)
	}
	closing := owner.BeginClose(frame.ErrClosed)
	select {
	case <-entered:
	case <-ctx.Done():
		close(finish)
		t.Fatal("consumer cleanup did not begin")
	}
	if err := consumer.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-providerClosed:
		t.Error("late proof released a provider still used by running cleanup")
	default:
	}
	close(finish)
	if outcome, err := closing.WaitReleased(ctx); err != nil || !outcome.Released {
		t.Fatalf("cleanup did not join its original close: %+v, %v", outcome, err)
	}
	select {
	case <-providerClosed:
	default:
		t.Fatal("duplicate dependency retained a released consumer")
	}
}

func TestRetirementReleasesSharedProvidersExactlyOnce(t *testing.T) {
	ctx := testContext(t)
	owner := frame.NewLifetime(context.Background())
	var closed atomic.Int64
	provider, err := owner.Own(frame.Resource{Name: "shared", Close: func(context.Context) (frame.ReleaseState, error) {
		closed.Add(1)
		return frame.ResourceReleased, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var retained *frame.ResourceHandle
	for index := range 128 {
		consumer, err := owner.Own(frame.Resource{Name: fmt.Sprintf("consumer-%d", index), Close: func(context.Context) (frame.ReleaseState, error) {
			if index == 0 {
				return frame.ResourceUnconfirmed, nil
			}
			return frame.ResourceReleased, nil
		}}, provider)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			retained = consumer
		}
	}
	closing := owner.BeginClose(frame.ErrClosed)
	if outcome, _ := closing.Wait(ctx); outcome.Released || closed.Load() != 0 {
		t.Fatalf("unconfirmed consumer lost its provider: %+v", outcome)
	}
	if err := retained.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	if outcome, err := closing.WaitReleased(ctx); err != nil || !outcome.Released || closed.Load() != 1 {
		t.Fatalf("provider was skipped or released twice: %+v, %v, closes=%d", outcome, err, closed.Load())
	}
}

func BenchmarkLifetimeDependencyChain(b *testing.B) {
	for _, size := range []int{256, 1024, 4096} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			for b.Loop() {
				owner, err := frame.NewLifetimeWithLimits(context.Background(), frame.LifetimeLimits{MaxResources: size})
				if err != nil {
					b.Fatal(err)
				}
				var dependencies []*frame.ResourceHandle
				for range size {
					handle, err := owner.Own(frame.Resource{Name: "chain", Close: func(context.Context) (frame.ReleaseState, error) {
						return frame.ResourceReleased, nil
					}}, dependencies...)
					if err != nil {
						b.Fatal(err)
					}
					dependencies = []*frame.ResourceHandle{handle}
				}
				if outcome, err := owner.Close(context.Background()); err != nil || !outcome.Released {
					b.Fatalf("close: %+v, %v", outcome, err)
				}
			}
		})
	}
}
