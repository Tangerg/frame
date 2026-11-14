package distribution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type analyzerDriver struct{ prepared int }

func (*analyzerDriver) Profile() framehost.Profile { return "example-analyzer" }
func (d *analyzerDriver) Prepare(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
	d.prepared++
	return nil, nil
}
func (*analyzerDriver) ValidateArchiveUnit(unit Unit) error {
	if len(unit.Resources) != 1 || unit.Resources[0] != "rules.txt" {
		return errors.New("analyzer requires rules.txt")
	}
	return nil
}

func TestExternalProfileArchiveNeedsItsHostDriver(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rules.txt"), []byte("rules"), 0600); err != nil {
		t.Fatal(err)
	}
	driver := new(analyzerDriver)
	manifest := Manifest{SchemaVersion: ManifestSchemaVersion, Package: "example", Version: "1", Artifacts: []Artifact{{Path: "rules.txt"}}, Units: []Unit{{ID: "check", Profile: driver.Profile(), Resources: []string{"rules.txt"}}}}
	var buf bytes.Buffer
	if _, err := Build(ctx, root, manifest, &buf); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "example.zip")
	if err := os.WriteFile(name, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	digest := value.Hash(buf.Bytes())
	if _, err := VerifyArchive(ctx, name, digest, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	gate := framehost.NewAdmission()
	cache := t.TempDir()
	t.Cleanup(func() {
		if err := removeSealed(cache); err != nil {
			t.Error(err)
		}
	})
	store, err := Open(Config{Directory: cache, Admission: gate, Trust: func(context.Context, Source, Manifest, Lock) error { return nil }, Retained: func(context.Context, value.Digest) (bool, error) { return false, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	lease, err := store.ImportArchiveHeld(ctx, name, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	host, err := framehost.New(ctx, framehost.Config{Authority: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(ctx)
	config := Installation{ID: "analysis", Enabled: true}
	if _, err = lease.Install(ctx, host, config); !errors.Is(err, framehost.ErrUnavailable) {
		t.Fatalf("unknown execution profile: %v", err)
	}
	if err = host.RegisterDriver(driver); err != nil {
		t.Fatal(err)
	}
	record, err := lease.Install(ctx, host, config)
	if err != nil {
		t.Fatal(err)
	}
	if driver.prepared != 0 {
		t.Fatal("inspection or installation executed code")
	}
	plan, err := host.PlanUnit(ctx, "", framehost.UnitRef{InstallationID: record.ID, Release: record.Release, UnitID: "check"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.ActivatePlan(ctx, "", plan); err != nil {
		t.Fatal(err)
	}
	if driver.prepared != 1 {
		t.Fatalf("execution count %d", driver.prepared)
	}
}
