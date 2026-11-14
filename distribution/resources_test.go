package distribution

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

type resourceCapacityDriver struct{ ResourceDriver }

func (d resourceCapacityDriver) Prepare(ctx context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
	for {
		_, err := setup.Cleanup("occupied", func() error { return nil })
		if errors.Is(err, frame.ErrCapacity) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return d.ResourceDriver.Prepare(ctx, setup)
}

func TestResourceDriverReservesCleanupBeforeOpeningContent(t *testing.T) {
	ctx := t.Context()
	_, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	driver, err := NewResourceDriver(store)
	if err != nil {
		t.Fatal(err)
	}
	host, err := framehost.New(ctx, framehost.Config{Authority: store.admission, Drivers: []framehost.Driver{resourceCapacityDriver{driver}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := host.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	lease, err := store.Acquire(ctx, release.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	record, err := lease.Install(ctx, host, Installation{ID: "theme", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	plan, err := host.PlanUnit(ctx, "", framehost.UnitRef{InstallationID: record.ID, Release: record.Release, UnitID: "theme"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ActivatePlan(ctx, "", plan); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("content was opened before release capacity was reserved: %v", err)
	}
}
