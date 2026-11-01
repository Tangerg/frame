package cancellation

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(buffer []byte) (int, error) { return f(buffer) }

func TestErrPreservesOrdinaryCategoryWithoutDuplicatingIt(t *testing.T) {
	if err := Err(t.Context()); err != nil {
		t.Fatalf("active context = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Err(ctx); err != context.Canceled {
		t.Fatalf("ordinary cancellation = %v, want its original category", err)
	}
}

func TestReaderReportsACancellationThatLandedDuringTheRead(t *testing.T) {
	t.Parallel()

	cause := errors.New("read canceled")
	ctx, cancel := context.WithCancelCause(t.Context())
	source := readerFunc(func(buffer []byte) (int, error) {
		cancel(cause)
		return copy(buffer, "data"), nil
	})

	buffer := make([]byte, 8)
	read, err := Reader(ctx, source).Read(buffer)
	if read != len("data") || !errors.Is(err, cause) {
		t.Fatalf("Read = (%d, %v), want %d bytes and the cancellation cause", read, err, len("data"))
	}
}

func TestReaderRefusesBeforeTouchingAnAlreadyCanceledSource(t *testing.T) {
	t.Parallel()

	cause := errors.New("already canceled")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	touched := false
	source := readerFunc(func([]byte) (int, error) {
		touched = true
		return 0, nil
	})

	read, err := Reader(ctx, source).Read(make([]byte, 8))
	switch {
	case touched:
		t.Fatal("Reader touched a source whose context was already canceled")
	case read != 0 || !errors.Is(err, cause) || !errors.Is(err, context.Canceled):
		t.Fatalf("Read = (%d, %v), want the cancellation category and cause", read, err)
	}
}

func TestReaderPreservesExpiredDeadlineCauseWithoutReading(t *testing.T) {
	cause := errors.New("file read deadline expired")
	ctx, cancel := context.WithDeadlineCause(t.Context(), time.Unix(0, 0), cause)
	defer cancel()
	source := readerFunc(func([]byte) (int, error) {
		t.Fatal("expired read touched the source")
		return 0, nil
	})
	if n, err := Reader(ctx, source).Read(make([]byte, 1)); n != 0 || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
		t.Fatalf("expired read lost deadline outcomes: (%d, %v)", n, err)
	}
}

func TestReaderPreservesCancellationAndConcurrentFailure(t *testing.T) {
	for _, failure := range []error{errors.New("source failed"), io.EOF} {
		t.Run(failure.Error(), func(t *testing.T) {
			cause := errors.New("read abandoned")
			ctx, cancel := context.WithCancelCause(t.Context())
			source := readerFunc(func(buffer []byte) (int, error) {
				cancel(cause)
				return copy(buffer, "data"), failure
			})
			buffer := make([]byte, 8)
			n, err := Reader(ctx, source).Read(buffer)
			if n != 4 || string(buffer[:n]) != "data" || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || !errors.Is(err, failure) {
				t.Fatalf("concurrent read outcomes were lost: (%d, %v)", n, err)
			}
		})
	}
}

func TestReaderPreservesOrdinaryEOF(t *testing.T) {
	source := readerFunc(func([]byte) (int, error) { return 0, io.EOF })
	if n, err := Reader(t.Context(), source).Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("uncanceled read changed EOF semantics: (%d, %v)", n, err)
	}
}
