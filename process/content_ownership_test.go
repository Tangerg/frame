package process_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type contentSource func(context.Context, frame.UnitBinding) (process.Program, error)

func (s contentSource) Program(ctx context.Context, binding frame.UnitBinding) (process.Program, error) {
	return s(ctx, binding)
}

type saturatedProcessDriver struct{ process *process.Driver }

func (d saturatedProcessDriver) Profile() frame.Profile { return d.process.Profile() }

func (d saturatedProcessDriver) Prepare(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	for ctx.Err() == nil {
		_, err := setup.Cleanup("occupied capacity", func() error { return nil })
		if errors.Is(err, frame.ErrCapacity) {
			return d.process.Prepare(ctx, setup)
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, ctx.Err()
}

func contentHost(t *testing.T, source contentSource, saturated bool) (*frame.Host, frame.UnitRef) {
	t.Helper()
	gate := frame.NewAdmission()
	store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "state.sqlite"), Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	processDriver, err := process.New(process.Config{
		Runner: &exitCapacityRunner{}, Store: store, Source: source,
		CloseTimeout: time.Second, MaxFrameBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	var driver frame.Driver = processDriver
	if saturated {
		driver = saturatedProcessDriver{process: processDriver}
	}
	host, err := frame.NewHost(t.Context(), frame.HostConfig{
		Admission: gate, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store,
		Drivers: []frame.Driver{driver},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = host.Close(ctx)
	})
	plugin, err := frame.Define(frame.Definition{
		Revision: value.Hash([]byte("content-ownership")),
		Units:    []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "worker", Profile: frame.LocalProcess}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(t.Context(), frame.InstallationConfig{ID: "provider", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return host, frame.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "worker"}
}

func TestContentCapacityIsReservedBeforeSourceAcquisition(t *testing.T) {
	var acquisitions atomic.Int32
	host, unit := contentHost(t, func(context.Context, frame.UnitBinding) (process.Program, error) {
		acquisitions.Add(1)
		return process.Program{}, errors.New("source must not run without ownership capacity")
	}, true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := host.ActivateUnit(ctx, "application", unit); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("saturated resource admission: %v", err)
	}
	if acquisitions.Load() != 0 {
		t.Fatal("source acquired content before its cleanup capacity was reserved")
	}
}

func TestSourceFailureRetainsItsReturnedContentLease(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "content-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	resolutionFailure := errors.New("program resolution failed after content acquisition")
	releaseFailure := errors.New("content lease release temporarily failed")
	var releases atomic.Int32
	host, unit := contentHost(t, func(context.Context, frame.UnitBinding) (process.Program, error) {
		return process.Program{ReleaseContent: func() error {
			releases.Add(1)
			return releaseFailure
		}}, resolutionFailure
	}, false)
	if _, err := host.ActivateUnit(t.Context(), "application", unit); !errors.Is(err, resolutionFailure) {
		t.Fatalf("source failure was lost: %v", err)
	}
	if outcome, err := host.Close(t.Context()); outcome.Released || !errors.Is(err, releaseFailure) {
		t.Fatalf("failed content cleanup lost its owner: %+v %v", outcome, err)
	}
	if _, err := file.Stat(); err != nil {
		t.Fatalf("fixture content closed before release: %v", err)
	}
	if outcome, err := host.Close(t.Context()); outcome.Released || !errors.Is(err, releaseFailure) || releases.Load() != 1 {
		t.Fatalf("repeated wait replaced the original cleanup: %+v releases=%d error=%v", outcome, releases.Load(), err)
	}
}

func TestSourceFailureReleasesReturnedContentExactlyOnce(t *testing.T) {
	failure := errors.New("program resolution failed after acquisition")
	var releases atomic.Int32
	host, unit := contentHost(t, func(context.Context, frame.UnitBinding) (process.Program, error) {
		return process.Program{ReleaseContent: func() error { releases.Add(1); return nil }}, failure
	}, false)
	if _, err := host.ActivateUnit(t.Context(), "application", unit); !errors.Is(err, failure) {
		t.Fatalf("source failure: %v", err)
	}
	if outcome, err := host.Close(t.Context()); !outcome.Released || releases.Load() != 1 {
		t.Fatalf("content release ownership: %+v releases=%d error=%v", outcome, releases.Load(), err)
	}
}

func TestSourceFailureWithoutContentLeavesNoResource(t *testing.T) {
	failure := errors.New("program resolution refused")
	host, unit := contentHost(t, func(context.Context, frame.UnitBinding) (process.Program, error) {
		return process.Program{}, failure
	}, false)
	if _, err := host.ActivateUnit(t.Context(), "application", unit); !errors.Is(err, failure) {
		t.Fatalf("source failure: %v", err)
	}
	if outcome, err := host.Close(t.Context()); !outcome.Released {
		t.Fatalf("empty content reservation retained a resource: %+v %v", outcome, err)
	}
}
