package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestCopiedWatchReleasesItsOriginalSubscription(t *testing.T) {
	host, err := frame.NewHost(context.Background(), frame.HostConfig{Limits: frame.HostLimits{MaxWatches: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, original, err := host.Watch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	copied := *original
	copied.Close()
	_, replacement, err := host.Watch(context.Background())
	if err != nil {
		t.Fatalf("copied handle closed the stream but retained its original subscription: %v", err)
	}
	original.Close()
	copied.Close()
	if _, extra, err := host.Watch(context.Background()); !errors.Is(err, frame.ErrCapacity) || extra != nil {
		t.Fatalf("original handle released another subscription: %v", err)
	}
	select {
	case _, open := <-original.Changes():
		if open {
			t.Fatal("copied handle did not close its original stream")
		}
	default:
		t.Fatal("original stream remains open")
	}
	replacement.Close()
	if outcome, err := host.Close(testContext(t)); err != nil || !outcome.Released {
		t.Fatalf("closed watch handles retained the host: %+v, %v", outcome, err)
	}
}
