package distribution

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestOpenDefaultsEachUnsetLimitIndependently(t *testing.T) {
	config := ownerConfig(t.TempDir())
	config.Limits = Limits{MaxEntries: 1}
	store, err := Open(config)
	if err != nil {
		t.Fatalf("narrowing one limit refused the others' defaults: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	want := DefaultLimits()
	want.MaxEntries = 1
	if store.limits != want {
		t.Fatalf("resolved limits %+v, want %+v", store.limits, want)
	}
	config = ownerConfig(t.TempDir())
	config.Limits = Limits{MaxArchiveBytes: -1}
	if _, err := Open(config); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("negative limit admitted: %v", err)
	}
}
