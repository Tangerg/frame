package frame_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"

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
	var retained frame.ResourceHandle
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

func TestCloseWaitJoinsCleanupUnlockedByLateProof(t *testing.T) {
	for _, independentPending := range []bool{false, true} {
		name := "all resources released"
		if independentPending {
			name = "independent proof still pending"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner := frame.NewLifetime(t.Context())
				var providerCloses, consumerCloses atomic.Int64
				entered, finish := make(chan struct{}), make(chan struct{})
				provider, err := owner.Own(frame.Resource{Name: "provider", Close: func(context.Context) (frame.ReleaseState, error) {
					providerCloses.Add(1)
					close(entered)
					<-finish
					return frame.ResourceReleased, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				consumer, err := owner.Own(frame.Resource{Name: "consumer", Close: func(context.Context) (frame.ReleaseState, error) {
					consumerCloses.Add(1)
					return frame.ResourceUnconfirmed, nil
				}}, provider)
				if err != nil {
					t.Fatal(err)
				}
				var independent frame.ResourceHandle
				if independentPending {
					independent, err = owner.Own(frame.Resource{Name: "independent", Close: func(context.Context) (frame.ReleaseState, error) {
						return frame.ResourceUnconfirmed, nil
					}})
					if err != nil {
						t.Fatal(err)
					}
				}
				closing := owner.BeginClose(frame.ErrClosed)
				first, err := closing.Wait(t.Context())
				if first.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) || providerCloses.Load() != 0 {
					t.Fatalf("stationary proof did not settle as pending: %+v, %v", first, err)
				}
				if _, err := closing.Wait(t.Context()); !errors.Is(err, frame.ErrUnconfirmedRelease) {
					t.Fatalf("observing pending proof changed its conclusion: %v", err)
				}
				if err := consumer.ConfirmReleased(nil); err != nil {
					t.Fatal(err)
				}
				type result struct {
					outcome frame.CloseOutcome
					err     error
				}
				closed := make(chan result, 1)
				go func() {
					outcome, err := closing.Wait(t.Context())
					closed <- result{outcome: outcome, err: err}
				}()
				<-entered
				synctest.Wait()
				select {
				case result := <-closed:
					t.Errorf("wait returned before the original provider finished: %+v, %v", result.outcome, result.err)
					closed <- result
				default:
				}
				wait, cancel := context.WithCancel(t.Context())
				cancel()
				if outcome, err := closing.Wait(wait); outcome.Released || !errors.Is(err, context.Canceled) {
					t.Errorf("cancelled wait invented release: %+v, %v", outcome, err)
				}
				close(finish)
				final := <-closed
				if final.outcome.ID != first.ID || final.outcome.Released == independentPending {
					t.Errorf("late proof changed close identity or pending evidence: %+v, %v", final.outcome, final.err)
				}
				if independentPending {
					if !errors.Is(final.err, frame.ErrUnconfirmedRelease) {
						t.Errorf("stationary independent proof did not return promptly: %v", final.err)
					}
					if err := independent.ConfirmReleased(nil); err != nil {
						t.Fatal(err)
					}
				} else if final.err != nil {
					t.Error(final.err)
				}
				if outcome, err := closing.WaitReleased(t.Context()); err != nil || !outcome.Released || providerCloses.Load() != 1 || consumerCloses.Load() != 1 {
					t.Fatalf("late proof repeated or abandoned original cleanup: %+v, %v, providers=%d, consumers=%d", outcome, err, providerCloses.Load(), consumerCloses.Load())
				}
			})
		})
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
				var dependencies []frame.ResourceHandle
				for range size {
					handle, err := owner.Own(frame.Resource{Name: "chain", Close: func(context.Context) (frame.ReleaseState, error) {
						return frame.ResourceReleased, nil
					}}, dependencies...)
					if err != nil {
						b.Fatal(err)
					}
					dependencies = []frame.ResourceHandle{handle}
				}
				if outcome, err := owner.Close(context.Background()); err != nil || !outcome.Released {
					b.Fatalf("close: %+v, %v", outcome, err)
				}
			}
		})
	}
}
