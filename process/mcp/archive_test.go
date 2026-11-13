package mcp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Tangerg/frame/distribution"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

// An extension-profile archive can be inspected offline, but installation also
// needs its registered driver to validate the executable shape, without launch.
func TestInstallToolArchive(t *testing.T) {
	ctx := t.Context()
	gate := framehost.NewAdmission()
	state, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "state.db"), Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	client, err := runner.NewClient(filepath.Join(t.TempDir(), "unused"))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := process.New(process.Config{Runner: client, Store: state, Source: fixtureSource{}, CloseTimeout: time.Second, MaxFrameBytes: 1024, Protocol: Tools{MaxCatalogPages: 1}})
	if err != nil {
		t.Fatal(err)
	}
	host, err := framehost.New(ctx, framehost.Config{Admission: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(ctx)
	store, err := distribution.Open(distribution.Config{Directory: t.TempDir(), Admission: gate, Trust: func(context.Context, distribution.Source, distribution.Manifest, distribution.Lock) error { return nil }, Retained: func(context.Context, value.Digest) (bool, error) { return false, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "backend"), []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	variant := distribution.Variant{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: "backend"}
	for i, tc := range []struct {
		name     string
		variants []distribution.Variant
		want     error
	}{
		{"missing-entry", nil, distribution.ErrInvalid},
		{"registered", []distribution.Variant{variant}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			manifest := distribution.Manifest{SchemaVersion: distribution.ManifestSchemaVersion, Package: "tools", Version: "1", Artifacts: []distribution.Artifact{{Path: "backend", Executable: true}}, Units: []distribution.Unit{{ID: "tools", Profile: ToolsProfile, Variants: tc.variants}}}
			if _, err := distribution.Build(ctx, root, manifest, &archive); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "plugin.zip")
			if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			digest := value.Hash(archive.Bytes())
			lease, err := store.ImportArchiveHeld(ctx, path, digest)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := errors.Join(lease.Close(), store.Remove(ctx, lease.Snapshot().Digest)); err != nil {
					t.Error(err)
				}
			}()
			config := distribution.Installation{ID: "tools", Enabled: true}
			if i == 0 {
				if _, err := lease.Install(ctx, host, config); !errors.Is(err, framehost.ErrUnavailable) {
					t.Fatalf("unregistered driver: %v", err)
				}
				if err := host.RegisterDriver(driver); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := lease.Install(ctx, host, config); !errors.Is(err, tc.want) {
				t.Fatalf("install: %v, want %v", err, tc.want)
			}
		})
	}
}
