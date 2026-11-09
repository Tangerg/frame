package native

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"testing"

	"github.com/Tangerg/frame"
)

func TestUnlinkedViewReleasesItsOriginalFileAndProviders(t *testing.T) {
	workbench, _, output, reference, ctx := authorWorkbench(t)
	prepared, err := workbench.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err := prepared.Acquire(ctx, authorOwner(t), reference)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(view.file.Name()); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(ctx); err != nil {
		t.Fatalf("already absent path prevented release: %v", err)
	}
	if _, err := view.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("original file descriptor remained open: %v", err)
	}
	if err := workbench.Host.Uninstall(ctx, Installation); err != nil {
		t.Fatalf("released file retained its providers: %v", err)
	}
	if err := workbench.Host.WaitRetired(ctx, Installation); err != nil {
		t.Fatal(err)
	}
	assertDirectoryEmpty(t, output)
}

func TestViewReaderRejectsItsFencedOriginalGrant(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	workbench, _, _, reference, ctx := authorWorkbench(t)
	prepared, err := workbench.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err := prepared.Acquire(ctx, authorOwner(t), reference)
	if err != nil {
		t.Fatal(err)
	}
	workbench.Host.Admission().FenceScope(workbench.scope.ID())
	called := false
	err = view.WithReader(ctx, func(context.Context, io.Reader) error { called = true; return nil })
	if called || !errors.Is(err, frame.ErrFenced) && !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("fenced grant reached a new reader: called=%v, %v", called, err)
	}
}
