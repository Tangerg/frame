package distribution

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestStoreCopiesShareTheOriginalDirectoryClose(t *testing.T) {
	store, err := Open(ownerConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	copied := store
	t.Cleanup(func() {
		if err := copied.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := copied.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Path(value.Hash([]byte("release"))); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("copy left another directory authority open: %v", err)
	}
	if _, err := store.Digests(t.Context()); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("copy bypassed the original directory close: %v", err)
	}
}

func TestResourceDriverRejectsAnUnissuedStore(t *testing.T) {
	if driver, err := NewResourceDriver(Store{}); !errors.Is(err, frame.ErrInvalid) || driver.store != nil {
		t.Fatalf("unissued store became a resource driver: %v", err)
	}
}

func TestStoreHandleReplacementRetainsIssuedLeasesAndDrivers(t *testing.T) {
	for _, replacement := range []struct {
		name string
		open func(*testing.T) Store
	}{
		{"zero", func(*testing.T) Store { return Store{} }},
		{"another directory", func(t *testing.T) Store { return storeFixture(t, nil) }},
	} {
		t.Run(replacement.name, func(t *testing.T) {
			_, _, archive, _ := packageFixture(t)
			store := storeFixture(t, nil)
			original := store
			release := installArchive(t, store, archive)
			lease, err := store.Acquire(t.Context(), release.Digest)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lease.Close() })
			driver, err := NewResourceDriver(store)
			if err != nil {
				t.Fatal(err)
			}
			store = replacement.open(t)
			content, err := lease.Read(t.Context(), "theme.css")
			if err != nil || string(content) != "body { color: blue; }\n" {
				t.Fatalf("handle replacement redirected the original lease: %q, %v", content, err)
			}
			host, err := framehost.New(t.Context(), framehost.Config{Authority: original.admission})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := host.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			if err := host.RegisterDriver(driver); err != nil {
				t.Fatal(err)
			}
			record, err := lease.Install(t.Context(), host, Installation{ID: "original", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := host.PlanUnit(t.Context(), "", framehost.UnitRef{InstallationID: record.ID, Release: record.Release, UnitID: "theme"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := host.ActivatePlan(t.Context(), "", plan); err != nil {
				t.Fatalf("handle replacement redirected the original resource driver: %v", err)
			}
			if err := store.Close(t.Context()); err != nil {
				t.Fatalf("replacement retained another directory's claims: %v", err)
			}
		})
	}
}

func TestUnissuedStoreCannotStartAnOperation(t *testing.T) {
	var store Store
	if _, err := store.Path(value.Hash([]byte("release"))); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("unissued handle admitted a directory operation: %v", err)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatalf("empty handle retained a directory: %v", err)
	}
}
