package frame_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

func TestCancellationGoexitDoesNotAbandonOriginalRetirement(t *testing.T) {
	lifetime := frame.NewLifetime(context.Background())
	var canceled, closed atomic.Int64
	for _, resource := range []frame.Resource{
		{Name: "interrupted cancellation", Cancel: func(error) { runtime.Goexit() }, Close: func(context.Context) (frame.ReleaseState, error) {
			closed.Add(1)
			return frame.ResourceReleased, nil
		}},
		{Name: "remaining cancellation", Cancel: func(error) { canceled.Add(1) }, Close: func(context.Context) (frame.ReleaseState, error) {
			closed.Add(1)
			return frame.ResourceReleased, nil
		}},
	} {
		if _, err := lifetime.Own(resource); err != nil {
			t.Fatal(err)
		}
	}
	outcome, err := lifetime.Close(testContext(t))
	if !outcome.Released || !errors.Is(err, frame.ErrCloseFailed) || canceled.Load() != 1 || closed.Load() != 2 {
		t.Fatalf("interrupted cancellation abandoned retirement: %+v, error=%v, canceled=%d, closed=%d", outcome, err, canceled.Load(), closed.Load())
	}
}

func TestCleanupGoexitSettlesWithoutInventingReleaseProof(t *testing.T) {
	lifetime := frame.NewLifetime(context.Background())
	var calls atomic.Int64
	resource, err := lifetime.Own(frame.Resource{Name: "interrupted cleanup", Close: func(context.Context) (frame.ReleaseState, error) {
		calls.Add(1)
		runtime.Goexit()
		return frame.ResourceReleased, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	closing := lifetime.BeginClose(frame.ErrClosed)
	outcome, err := closing.Wait(testContext(t))
	if outcome.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("ended cleanup did not settle as unconfirmed: %+v, %v", outcome, err)
	}
	if err := resource.ConfirmReleased(nil); err != nil {
		t.Fatal(err)
	}
	outcome, err = closing.WaitReleased(testContext(t))
	if !outcome.Released || calls.Load() != 1 || !errors.Is(err, frame.ErrCloseFailed) {
		t.Fatalf("original release proof could not finish cleanup: %+v, %v, calls=%d", outcome, err, calls.Load())
	}
}

func TestPreparationGoexitRetiresItsOriginalInstance(t *testing.T) {
	for _, phase := range []string{"setup", "activation"} {
		t.Run(phase, func(t *testing.T) {
			host, err := frame.NewHost(context.Background(), frame.HostConfig{})
			if err != nil {
				t.Fatal(err)
			}
			var closed atomic.Int64
			plugin := installNative(t, host, phase, frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
				Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
					if _, err := setup.Cleanup("original resource", func() error { closed.Add(1); return nil }); err != nil {
						return nil, err
					}
					if phase == "activation" {
						return nil, setup.OnActivate(func(context.Context) error { runtime.Goexit(); return nil })
					}
					runtime.Goexit()
					return nil, nil
				},
			})
			_, err = host.ActivateUnit(testContext(t), "", frame.UnitRef{InstallationID: frame.InstallationID(phase), Release: plugin.Revision(), UnitID: "main"})
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("interrupted %s did not conclude preparation: %v", phase, err)
			}
			if err := host.WaitRetired(testContext(t), frame.InstallationID(phase)); err != nil {
				t.Fatal(err)
			}
			if closed.Load() != 1 {
				t.Fatalf("interrupted preparation retained its original resource: %d", closed.Load())
			}
			if outcome, err := host.Close(testContext(t)); err != nil || !outcome.Released {
				t.Fatalf("interrupted preparation blocked host retirement: %+v, %v", outcome, err)
			}
		})
	}
}

func TestResourcePanicRetainsOriginalErrorCause(t *testing.T) {
	for _, phase := range []string{"cancel", "close"} {
		t.Run(phase, func(t *testing.T) {
			original := errors.New("original resource failure")
			lifetime := frame.NewLifetime(context.Background())
			resource := frame.Resource{Name: "original", Close: func(context.Context) (frame.ReleaseState, error) {
				return frame.ResourceReleased, nil
			}}
			if phase == "cancel" {
				resource.Cancel = func(error) { panic(original) }
			} else {
				resource.Close = func(context.Context) (frame.ReleaseState, error) { panic(original) }
			}
			handle, err := lifetime.Own(resource)
			if err != nil {
				t.Fatal(err)
			}
			_, err = lifetime.Close(testContext(t))
			if !errors.Is(err, original) {
				t.Errorf("%s panic lost its original cause: %v", phase, err)
			}
			if err := handle.ConfirmReleased(nil); err != nil {
				t.Fatal(err)
			}
			_, _ = lifetime.BeginClose(frame.ErrClosed).WaitReleased(testContext(t))
		})
	}
}

func TestPreparationPanicRetainsOriginalErrorCause(t *testing.T) {
	for _, phase := range []string{"setup", "activation"} {
		t.Run(phase, func(t *testing.T) {
			host := testHost(t, frame.HostConfig{})
			original := errors.New("original preparation failure")
			plugin := installNative(t, host, phase, frame.Unit{
				Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
				Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
					if phase == "activation" {
						return nil, setup.OnActivate(func(context.Context) error { panic(original) })
					}
					panic(original)
				},
			})
			_, err := host.ActivateUnit(testContext(t), "", frame.UnitRef{InstallationID: frame.InstallationID(phase), Release: plugin.Revision(), UnitID: "main"})
			if !errors.Is(err, original) {
				t.Fatalf("%s panic lost its original cause: %v", phase, err)
			}
		})
	}
}
