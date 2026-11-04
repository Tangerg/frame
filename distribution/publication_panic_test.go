package distribution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/value"
)

func TestCachedTrustPanicReleasesOriginalLease(t *testing.T) {
	_, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	file := filepath.Join(t.TempDir(), "plugin.zip")
	if err := os.WriteFile(file, archive, privateFileMode); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("cached trust panicked")
	checks := 0
	store.trust = func(context.Context, Source, Manifest, Lock) error {
		checks++
		if checks == 2 {
			panic(refusal)
		}
		return nil
	}
	func() {
		defer func() {
			if cause := recover(); cause != refusal {
				t.Errorf("original trust panic = %v", cause)
			}
		}()
		_, _ = store.ImportArchiveHeld(t.Context(), file, value.Hash(archive))
	}()
	if checks != 2 {
		t.Fatalf("cached trust was not reached: %d checks", checks)
	}
	if err := store.Remove(t.Context(), release.Digest); err != nil {
		t.Fatalf("trust panic leaked its private lease: %v", err)
	}
}
