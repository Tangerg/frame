package frame_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/frame"
)

func TestLifetimeWaitersRejectMissingContexts(t *testing.T) {
	for _, held := range []bool{false, true} {
		owner := frame.NewLifetime(context.Background())
		t.Cleanup(func() {
			if _, err := owner.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		if held {
			hold, err := owner.Hold()
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Finish()
		}
		exit := owner.BeginClose(frame.ErrClosed)
		for _, wait := range []struct {
			name string
			call func(context.Context) error
		}{
			{"holds", owner.AwaitUnheld},
			{"settlement", func(ctx context.Context) error { _, err := exit.Wait(ctx); return err }},
			{"release", func(ctx context.Context) error { _, err := exit.WaitReleased(ctx); return err }},
		} {
			for _, ctx := range []context.Context{nil, (*absentContext)(nil)} {
				if err := wait.call(ctx); !errors.Is(err, frame.ErrInvalid) {
					t.Errorf("%s wait admitted missing context (held=%t): %v", wait.name, held, err)
				}
			}
		}
	}
}

func TestLifetimeCloseKeepsOriginalCancellation(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, parentCancels := range []bool{false, true} {
		name := "local"
		if parentCancels {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			parent, cancelParent := context.WithCancelCause(context.Background())
			defer cancelParent(frame.ErrClosed)
			owner := frame.NewLifetime(parent)
			work, err := owner.Enter(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				work.Finish()
				if outcome, err := owner.Close(context.Background()); !outcome.Released || err != nil {
					t.Errorf("original close: %+v, %v", outcome, err)
				}
			}()
			originalContext := owner.Context()
			resourceNames := []string{"original resource", "late resource"}
			type cancellation struct {
				resource string
				cause    error
			}
			notifications := make(chan cancellation, len(resourceNames))
			var cleanups atomic.Int32
			resource := func(name string) frame.Resource {
				return frame.Resource{
					Name:   name,
					Cancel: func(cause error) { notifications <- cancellation{name, cause} },
					Close: func(context.Context) (frame.ReleaseState, error) {
						cleanups.Add(1)
						return frame.ResourceReleased, nil
					},
				}
			}
			if _, err := owner.Own(resource(resourceNames[0])); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("original cancellation")
			if parentCancels {
				cancelParent(cause)
			} else {
				owner.Cancel(cause)
			}
			exit := owner.BeginClose(frame.ErrClosed)
			if !errors.Is(context.Cause(originalContext), cause) {
				t.Errorf("close replaced the original context cause: %v", context.Cause(originalContext))
			}
			if !errors.Is(context.Cause(work.Context()), cause) {
				t.Errorf("work received another cancellation cause: %v", context.Cause(work.Context()))
			}
			if _, err := work.Own(resource(resourceNames[1])); err != nil {
				t.Fatal(err)
			}
			work.Finish()
			if outcome, err := exit.WaitReleased(t.Context()); !outcome.Released || err != nil || cleanups.Load() != int32(len(resourceNames)) {
				t.Fatalf("original close lost actual cleanup: %+v, cleanups=%d error=%v", outcome, cleanups.Load(), err)
			}
			for range resourceNames {
				select {
				case notification := <-notifications:
					if !errors.Is(notification.cause, cause) {
						t.Errorf("%s received another cancellation cause: %v", notification.resource, notification.cause)
					}
				default:
					t.Fatal("original cancellation notification was lost")
				}
			}
		})
	}
}

func TestLifetimeDeadlineKeepsParentCategoryAndCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cause := errors.New("original parent deadline")
		parent, cancel := context.WithTimeoutCause(context.Background(), time.Second, cause)
		defer cancel()
		owner := frame.NewLifetime(parent)
		work, err := owner.Enter(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			work.Finish()
			if outcome, err := owner.Close(context.Background()); !outcome.Released || err != nil {
				t.Errorf("original close: %+v, %v", outcome, err)
			}
		}()
		original := owner.Context()
		time.Sleep(time.Second)
		synctest.Wait()
		failure := frame.ContextError(original)
		if !errors.Is(failure, context.DeadlineExceeded) || !errors.Is(failure, cause) {
			t.Fatalf("parent deadline changed its original category or cause: %v", failure)
		}
		exit := owner.BeginClose(frame.ErrClosed)
		if !errors.Is(context.Cause(work.Context()), cause) {
			t.Fatalf("work lost the original deadline cause: %v", context.Cause(work.Context()))
		}
		work.Finish()
		if outcome, err := exit.WaitReleased(t.Context()); !outcome.Released || err != nil {
			t.Fatalf("original deadline exit: %+v, %v", outcome, err)
		}
	})
}

func TestCompletedReleaseWinsOverWaitCancellation(t *testing.T) {
	owner := frame.NewLifetime(context.Background())
	exit := owner.BeginClose(frame.ErrClosed)
	if _, err := exit.WaitReleased(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, wait := range []func(context.Context) (frame.CloseOutcome, error){exit.Wait, exit.WaitReleased} {
		if outcome, err := wait(ctx); err != nil || !outcome.Released {
			t.Fatalf("wait cancellation replaced original release evidence: %+v, %v", outcome, err)
		}
	}
}

func TestLifetimeRejectsAnInvalidCloseBeforeStartingExit(t *testing.T) {
	for _, ctx := range []context.Context{nil, (*absentContext)(nil)} {
		owner := frame.NewLifetime(context.Background())
		if _, err := owner.Close(ctx); !errors.Is(err, frame.ErrInvalid) {
			t.Errorf("invalid close admitted: %v", err)
		}
		if owner.IsClosing() {
			t.Error("invalid wait context started the original exit")
		}
		if _, err := owner.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
