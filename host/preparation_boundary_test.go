package host_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type preparationFailure string

func (e preparationFailure) Error() string { return string(e) }

const metadataFailure preparationFailure = "original metadata callback failure"

type panickingMetadataEncoder struct{ value.JSON }

func (panickingMetadataEncoder) MarshalJSON() ([]byte, error) { panic(metadataFailure) }

type panickingMetadataDecoder struct{ value.JSON }

func (*panickingMetadataDecoder) UnmarshalJSON([]byte) error { panic(metadataFailure) }

type exitingMetadataEncoder struct{ value.JSON }

func (exitingMetadataEncoder) MarshalJSON() ([]byte, error) {
	runtime.Goexit()
	return nil, nil
}

func TestMetadataCallbacksConcludeOriginalPreparation(t *testing.T) {
	encoded, err := value.ParseJSON([]byte(`"metadata"`))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("encoder panic", func(t *testing.T) {
		testMetadataPreparationFailure(t, panickingMetadataEncoder{encoded}, metadataFailure)
	})
	t.Run("decoder panic", func(t *testing.T) {
		testMetadataPreparationFailure(t, panickingMetadataDecoder{encoded}, metadataFailure)
	})
	t.Run("encoder goexit", func(t *testing.T) {
		testMetadataPreparationFailure(t, exitingMetadataEncoder{encoded}, nil)
	})
}

func metadataTestPoint[M any](t *testing.T, host framehost.Host) framehost.Point[M, string] {
	t.Helper()
	descriptor := []byte(`{"protocol":"metadata-boundary"}`)
	service, err := framehost.BindContract[string](framehost.ContractRef{Name: "metadata.value", Version: "1", Kind: framehost.ServiceKind, Mode: framehost.PortableMode, Digest: value.Hash(descriptor)}, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[M]("metadata.point", "1", []byte(`true`), service)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service, point); err != nil {
		t.Fatal(err)
	}
	return point
}

func testMetadataPreparationFailure[M any](t *testing.T, metadata M, cause error) {
	t.Helper()
	host := testHost(t, framehost.Config{})
	point := metadataTestPoint[M](t, host)
	var cleanups atomic.Int64
	plugin := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			if _, err := setup.Cleanup("original provider", func() error { cleanups.Add(1); return nil }); err != nil {
				return nil, err
			}
			return []framehost.ExportValue{framehost.Contribute("member", point, metadata, "value")}, nil
		},
	})
	_, err := host.Activate(testContext(t), source("provider", plugin, "main", "member", point.Ref()))
	if err == nil || errors.Is(err, context.DeadlineExceeded) || cause != nil && !errors.Is(err, cause) {
		t.Fatalf("metadata callback lost original preparation result: %v", err)
	}
	if err := host.WaitRetired(testContext(t), "provider"); err != nil {
		t.Fatal(err)
	}
	if cleanups.Load() != 1 {
		t.Fatalf("failed preparation did not release its original resource once: %d", cleanups.Load())
	}
	snapshot, err := host.Snapshot(testContext(t), point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("failed preparation published a partial contribution: %d, %v", len(candidates), err)
	}
}

type panickingExportDriver struct {
	contract framehost.Contract[string]
	cleanups atomic.Int64
}

func (*panickingExportDriver) Profile() framehost.Profile { return "test.export-validator" }
func (d *panickingExportDriver) Prepare(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
	if _, err := setup.Cleanup("original driver resource", func() error { d.cleanups.Add(1); return nil }); err != nil {
		return nil, err
	}
	return []framehost.ExportValue{framehost.Provide("service", d.contract, "value")}, nil
}
func (*panickingExportDriver) ValidateExport(string, framehost.ExportValue) error {
	panic(metadataFailure)
}

func TestExportValidatorPanicConcludesOriginalPreparation(t *testing.T) {
	contract, err := framehost.NewContract[string]("test.value", "1")
	if err != nil {
		t.Fatal(err)
	}
	driver := &panickingExportDriver{contract: contract}
	host := testHost(t, framehost.Config{Drivers: []framehost.Driver{driver}})
	if err := host.Register(contract); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "provider", framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "main", Profile: driver.Profile(), Exports: []framehost.ExportDeclaration{{Name: "service", Contract: contract.Ref()}}}})
	_, err = host.Activate(testContext(t), source("provider", plugin, "main", "service", contract.Ref()))
	if !errors.Is(err, metadataFailure) {
		t.Fatalf("driver validation panic lost its original cause: %v", err)
	}
	if err := host.WaitRetired(testContext(t), "provider"); err != nil {
		t.Fatal(err)
	}
	if driver.cleanups.Load() != 1 {
		t.Fatalf("driver validation panic retained its original resource: %d", driver.cleanups.Load())
	}
}

type observedMetadata struct {
	value.JSON
	encodes *atomic.Int64
}

func (m observedMetadata) MarshalJSON() ([]byte, error) {
	m.encodes.Add(1)
	return m.JSON.MarshalJSON()
}

func TestScopeFencedDuringSetupRefusesMetadataCallback(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	host := testHost(t, framehost.Config{})
	point := metadataTestPoint[observedMetadata](t, host)
	owner, grant := ownerAndScope(t, host)
	encoded, err := value.ParseJSON([]byte(`"metadata"`))
	if err != nil {
		t.Fatal(err)
	}
	var encodes atomic.Int64
	plugin := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			host.Admission().FenceScope(grant.ID())
			return []framehost.ExportValue{framehost.Contribute("member", point, observedMetadata{JSON: encoded, encodes: &encodes}, "value")}, nil
		},
	})
	use, err := host.AcquireUnit(testContext(t), framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"}, framehost.UseConfig{Owner: owner, Scope: grant})
	if use.Valid() || err == nil || encodes.Load() != 0 {
		t.Fatalf("fenced preparation ran metadata code: use=%v, encodes=%d, error=%v", use, encodes.Load(), err)
	}
}

type exitingPanicDiagnostic struct{ formatted *atomic.Int64 }

func (p exitingPanicDiagnostic) String() string {
	p.formatted.Add(1)
	runtime.Goexit()
	return ""
}

func TestPreparationPanicCannotExecuteItsDiagnosticPayload(t *testing.T) {
	host := testHost(t, framehost.Config{})
	var formatted, cleanups atomic.Int64
	plugin := installNative(t, host, "provider", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo},
		Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
			if _, err := setup.Cleanup("original provider", func() error { cleanups.Add(1); return nil }); err != nil {
				return nil, err
			}
			panic(exitingPanicDiagnostic{formatted: &formatted})
		},
	})
	_, err := host.ActivateUnit(testContext(t), "", framehost.UnitRef{InstallationID: "provider", Release: plugin.Revision(), UnitID: "main"})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || formatted.Load() != 0 {
		t.Fatalf("panic diagnostic escaped original preparation: formatted=%d, error=%v", formatted.Load(), err)
	}
	if err := host.WaitRetired(testContext(t), "provider"); err != nil {
		t.Fatal(err)
	}
	if cleanups.Load() != 1 {
		t.Fatalf("diagnostic interrupted original retirement: %d", cleanups.Load())
	}
}
