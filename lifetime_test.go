package frame_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/Tangerg/frame"
)

type lifetimeDiagnostic struct{ owner *frame.Lifetime }

func (e lifetimeDiagnostic) Error() string {
	_ = e.owner.Context()
	return strings.Repeat("界", 1000)
}

func TestCleanupDiagnosticsCanObserveTheirOwnerAndRemainValidJSON(t *testing.T) {
	for _, test := range []struct {
		name  string
		proof bool
	}{{"close", false}, {"late proof", true}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := testContext(t)
			owner := frame.NewLifetime(context.Background())
			resource, err := owner.Own(frame.Resource{Name: "original", Close: func(context.Context) (frame.ReleaseState, error) {
				if test.proof {
					return frame.ResourceUnconfirmed, nil
				}
				return frame.ResourceReleased, lifetimeDiagnostic{owner}
			}})
			if err != nil {
				t.Fatal(err)
			}
			completed := make(chan frame.CloseOutcome, 1)
			go func() {
				closing := owner.BeginClose(frame.ErrClosed)
				if test.proof {
					_, _ = closing.Wait(ctx)
					_ = resource.ConfirmReleased(lifetimeDiagnostic{owner})
				}
				outcome, _ := closing.WaitReleased(ctx)
				completed <- outcome
			}()
			select {
			case outcome := <-completed:
				if !outcome.Released || len(outcome.Failures) != 1 || !utf8.ValidString(outcome.Failures[0].Detail) || len(outcome.Failures[0].Detail) >= len(lifetimeDiagnostic{owner}.Error()) {
					t.Fatalf("cleanup diagnostic lost its bounded JSON representation: %+v", outcome)
				}
			case <-ctx.Done():
				t.Fatal("error formatting blocked its own lifetime")
			}
		})
	}
}

func TestUnconfirmedConsumerPinsProviderAndLateProofResumesOriginalClose(t *testing.T) {
	ctx := testContext(t)
	lifetime := frame.NewLifetime(context.Background())
	var providerClosed, consumerClosed, independentClosed atomic.Int64
	provider, err := lifetime.Own(frame.Resource{Name: "provider", Close: func(context.Context) (frame.ReleaseState, error) {
		providerClosed.Add(1)
		return frame.ResourceReleased, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := lifetime.Own(frame.Resource{Name: "consumer", Close: func(context.Context) (frame.ReleaseState, error) {
		consumerClosed.Add(1)
		return frame.ResourceUnconfirmed, errors.New("connection lost")
	}}, provider)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lifetime.Own(frame.Resource{Name: "independent", Close: func(context.Context) (frame.ReleaseState, error) {
		independentClosed.Add(1)
		return frame.ResourceReleased, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	closing := lifetime.BeginClose(frame.ErrClosed)
	first, err := closing.Wait(ctx)
	if !errors.Is(err, frame.ErrUnconfirmedRelease) || first.Released {
		t.Fatalf("unknown release became final: %+v %v", first, err)
	}
	if providerClosed.Load() != 0 || consumerClosed.Load() != 1 || independentClosed.Load() != 1 {
		t.Fatalf("incorrect close order: %d %d %d", providerClosed.Load(), consumerClosed.Load(), independentClosed.Load())
	}
	if err := consumer.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	final, err := closing.WaitReleased(ctx)
	if !final.Released || final.ID != first.ID || providerClosed.Load() != 1 || consumerClosed.Load() != 1 {
		t.Fatalf("late proof restarted or skipped original close: %+v %v", final, err)
	}
}

func TestCanceledWaiterDoesNotCancelOwnerAndLateAcquisitionIsCanceled(t *testing.T) {
	ctx := testContext(t)
	lifetime := frame.NewLifetime(context.Background())
	work, err := lifetime.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closing := lifetime.BeginClose(errors.New("owner shutdown"))
	waitCtx, cancel := context.WithCancel(ctx)
	cancel()
	if outcome, err := closing.Wait(waitCtx); outcome.Released || !errors.Is(err, context.Canceled) {
		t.Fatalf("wait result: %+v %v", outcome, err)
	}
	var canceled, closed atomic.Int64
	_, err = work.Own(frame.Resource{Name: "late", Cancel: func(error) { canceled.Add(1) }, Close: func(context.Context) (frame.ReleaseState, error) { closed.Add(1); return frame.ResourceReleased, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Load() != 1 {
		t.Fatal("late resource missed cancellation")
	}
	work.Finish()
	final, err := closing.WaitReleased(ctx)
	if err != nil || !final.Released || closed.Load() != 1 {
		t.Fatalf("owner failed to finish: %+v %v", final, err)
	}
	if _, err := lifetime.Enter(ctx); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("work admitted after seal: %v", err)
	}
}

func TestCloseFailureHistoryIsBoundedAndErrorsDoNotNegateReleasedEvidence(t *testing.T) {
	lifetime := frame.NewLifetime(context.Background())
	for range 80 {
		if _, err := lifetime.Own(frame.Resource{Name: "released with diagnostic", Close: func(context.Context) (frame.ReleaseState, error) {
			return frame.ResourceReleased, errors.New("diagnostic")
		}}); err != nil {
			t.Fatal(err)
		}
	}
	final, err := lifetime.Close(testContext(t))
	if !final.Released || err == nil || len(final.Failures) != 32 || final.OmittedFailures != 48 {
		t.Fatalf("incorrect bounded conclusion: %+v %v", final, err)
	}
}
