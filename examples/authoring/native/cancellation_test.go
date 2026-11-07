package native

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Tangerg/frame/examples/document"
)

func TestAuthoredSourceAndValidationPreserveCancellation(t *testing.T) {
	configuration, _, reference := authorFiles(t)
	root, err := os.OpenRoot(configuration.SourceDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	cause := errors.New("caller stopped the document task")
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancelCause(t.Context())
		category := context.Canceled
		if deadline {
			var stop context.CancelFunc
			ctx, stop = context.WithDeadlineCause(t.Context(), time.Now().Add(-time.Second), cause)
			defer stop()
			category = context.DeadlineExceeded
		} else {
			cancel(cause)
		}
		defer cancel(nil)
		_, sourceError := readSource(ctx, root, reference)
		validator := validationRule{violatedBy: func(document.Converted) bool { t.Fatal("cancelled validation ran"); return false }}
		_, validationError := validator.validate(ctx, document.Converted{})
		for _, err := range []error{sourceError, validationError} {
			if !errors.Is(err, category) || !errors.Is(err, cause) {
				t.Fatalf("cancellation lost category or cause: %v", err)
			}
		}
	}
}
