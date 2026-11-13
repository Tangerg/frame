package distribution

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestLeaseCopiesShareOneContentClaim(t *testing.T) {
	_, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	lease, err := store.Acquire(t.Context(), release.Digest)
	if err != nil {
		t.Fatal(err)
	}
	checkCopiedLeaseClaim(t, store, lease)
}

func TestPublishedLeaseCopiesShareOneContentClaim(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "new"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			_, _, archive, _ := packageFixture(t)
			store := storeFixture(t, nil)
			if cached {
				installArchive(t, store, archive)
			}
			path := filepath.Join(t.TempDir(), "plugin.zip")
			if err := os.WriteFile(path, archive, privateFileMode); err != nil {
				t.Fatal(err)
			}
			lease, err := store.ImportArchiveHeld(t.Context(), path, value.Hash(archive))
			if err != nil {
				t.Fatal(err)
			}
			checkCopiedLeaseClaim(t, store, lease)
		})
	}
}

func checkCopiedLeaseClaim(t *testing.T, store Store, original *Lease) {
	t.Helper()
	t.Cleanup(func() { _ = original.Close() })
	release := original.Snapshot()
	independent, err := store.Acquire(t.Context(), release.Digest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = independent.Close() })
	copied := *original
	var closers sync.WaitGroup
	for _, lease := range []*Lease{original, &copied} {
		closers.Go(func() {
			if err := lease.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	closers.Wait()
	for _, lease := range []*Lease{original, &copied} {
		if _, err := lease.Read(t.Context(), "theme.css"); !errors.Is(err, frame.ErrClosed) {
			t.Errorf("closed claim allowed a read: %v", err)
		}
		if _, err := lease.Directory(); !errors.Is(err, frame.ErrClosed) {
			t.Errorf("closed claim exposed its directory: %v", err)
		}
		if lease.Snapshot().Digest != release.Digest {
			t.Error("closing discarded the detached release identity")
		}
	}
	if err := store.Remove(t.Context(), release.Digest); !errors.Is(err, framehost.ErrInUse) {
		t.Fatalf("copied claim released an independent lease: %v", err)
	}
	if _, err := independent.Read(t.Context(), "theme.css"); err != nil {
		t.Fatalf("independent lease lost content: %v", err)
	}
	if err := independent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(t.Context(), release.Digest); err != nil {
		t.Fatalf("final claim retained content: %v", err)
	}
	closeStore(t, store)
}
