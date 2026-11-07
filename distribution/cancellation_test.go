package distribution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDistributionCancellationPreservesCategoryAndCause(t *testing.T) {
	source, manifest, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	cause := errors.New("request owner stopped")
	for _, category := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(category.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			if category == context.DeadlineExceeded {
				deadline, stop := context.WithDeadlineCause(t.Context(), time.Time{}, cause)
				defer stop()
				ctx = deadline
			} else {
				cancel(cause)
			}
			defer cancel(nil)
			for _, operation := range []struct {
				name string
				run  func() error
			}{
				{"bounded read", func() error { _, err := readBounded(ctx, strings.NewReader("kept"), 4); return err }},
				{"build", func() error { _, err := Build(ctx, source, manifest, io.Discard); return err }},
				{"directory import", func() error { _, err := store.ImportDirectory(ctx, source); return err }},
				{"cache inspection", func() error { _, err := store.Inspect(ctx, release.Digest); return err }},
				{"archive emission", func() error {
					contents := releaseContents{files: map[string][]byte{"entry": {}}, limits: DefaultLimits()}
					return contents.writeArchive(ctx, io.Discard, nil)
				}},
			} {
				t.Run(operation.name, func(t *testing.T) {
					if err := operation.run(); !errors.Is(err, category) || !errors.Is(err, cause) {
						t.Fatalf("cancellation lost %v or its cause: %v", category, err)
					}
				})
			}
		})
	}
}

type canceledRead struct {
	cancel context.CancelCauseFunc
	cause  error
	result error
}

func (r canceledRead) Read(buffer []byte) (int, error) {
	r.cancel(r.cause)
	return copy(buffer, "complete"), r.result
}

type canceledWrite struct {
	cancel context.CancelCauseFunc
	cause  error
	result error
	bytes.Buffer
}

func (w *canceledWrite) Write(data []byte) (int, error) {
	w.cancel(w.cause)
	if w.result != nil {
		return 0, w.result
	}
	return w.Buffer.Write(data)
}

func TestDistributionIOErrorsKeepCancellationWithoutReplacingCompletedResults(t *testing.T) {
	cause := errors.New("request owner stopped")
	failure := errors.New("original io failure")
	t.Run("read failure", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		_, err := readBounded(ctx, canceledRead{cancel: cancel, cause: cause, result: failure}, 16)
		if !errors.Is(err, failure) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatalf("read error lost its original evidence: %v", err)
		}
	})
	t.Run("completed read", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		data, err := readBounded(ctx, canceledRead{cancel: cancel, cause: cause, result: io.EOF}, 16)
		if err != nil || string(data) != "complete" {
			t.Fatalf("complete read was replaced by cancellation: %q, %v", data, err)
		}
	})
	for _, result := range []error{nil, failure} {
		name := "completed archive"
		if result != nil {
			name = "archive failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			output := &canceledWrite{cancel: cancel, cause: cause, result: result}
			contents := releaseContents{files: map[string][]byte{"entry": []byte("complete")}, limits: DefaultLimits()}
			err := contents.writeArchive(ctx, output, nil)
			if result == nil {
				if err != nil || output.Len() == 0 {
					t.Fatalf("completed archive was replaced by cancellation: %v", err)
				}
				return
			}
			if !errors.Is(err, failure) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
				t.Fatalf("archive error lost its original evidence: %v", err)
			}
		})
	}
}
