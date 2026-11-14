package distribution

import (
	"context"
	"errors"
	"testing"
)

func TestOpenDefaultsEachUnsetLimitIndependently(t *testing.T) {
	config := ownerConfig(t.TempDir())
	config.Limits = Limits{Entries: 1}
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
	want.Entries = 1
	if store.limits != want {
		t.Fatalf("resolved limits %+v, want %+v", store.limits, want)
	}
	config = ownerConfig(t.TempDir())
	config.Limits = Limits{ArchiveBytes: -1}
	if _, err := Open(config); !errors.Is(err, ErrCapacity) {
		t.Fatalf("negative limit admitted: %v", err)
	}
}
