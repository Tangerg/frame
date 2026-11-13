package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestLifetimeCopiesShareTheOriginalResourceAndCloseOwner(t *testing.T) {
	original := frame.NewLifetime(context.Background())
	copied := original
	closed := 0
	if _, err := copied.Cleanup("copied resource", func() error { closed++; return nil }); err != nil {
		t.Fatal(err)
	}
	first := original.BeginClose(frame.ErrClosed)
	if outcome, err := first.WaitReleased(t.Context()); err != nil || !outcome.Released || closed != 1 {
		t.Fatalf("copied resource escaped original owner: %#v %v, closed=%d", outcome, err, closed)
	}
	if copied.BeginClose(frame.ErrClosed).Outcome().ID != first.Outcome().ID {
		t.Fatal("copy originated another close operation")
	}
	copied = frame.Lifetime{}
	if _, err := copied.Enter(t.Context()); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("unissued lifetime accepted work: %v", err)
	}
	if !original.Valid() || original.BeginClose(frame.ErrClosed).Outcome().ID != first.Outcome().ID {
		t.Fatal("handle replacement changed the original owner")
	}
}
