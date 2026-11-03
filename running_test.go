package frame_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

func TestRunningStopJoinsOriginalRetirementAfterCanceledWait(t *testing.T) {
	host := testHost(t, frame.HostConfig{})
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var cleanups atomic.Int32
	plugin := installNative(t, host, "worker", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			_, err := setup.Cleanup("worker", func() error {
				cleanups.Add(1)
				close(entered)
				<-release
				return nil
			})
			return nil, err
		},
	})
	grant, err := host.Admission().OpenScope(t.Context(), "worker-scope")
	if err != nil {
		t.Fatal(err)
	}
	running, err := host.StartUnit(t.Context(), grant, frame.UnitRef{InstallationID: "worker", Release: plugin.Revision(), UnitID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := running.Stop(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stop wait: %v", err)
	}
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if err := running.Stop(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation replaced the original retirement: %v", err)
	}
	finished := make(chan error, 2)
	for range cap(finished) {
		go func() { finished <- running.Stop(t.Context()) }()
	}
	unblock()
	for range cap(finished) {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	if err := running.Stop(canceled); err != nil || cleanups.Load() != 1 {
		t.Fatalf("completed retirement changed: cleanups=%d error=%v", cleanups.Load(), err)
	}
}
