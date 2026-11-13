package document

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tangerg/frame/distribution"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestModuleArtifactURLsPreserveLiteralFileNames(t *testing.T) {
	directory := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	})
	store, err := distribution.Open(distribution.Config{
		Directory: filepath.Join(directory, "content"), Admission: framehost.NewAdmission(),
		Trust:    func(context.Context, distribution.Source, distribution.Manifest, distribution.Lock) error { return nil },
		Retained: func(context.Context, value.Digest) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	manifest := distribution.Manifest{SchemaVersion: distribution.ManifestSchemaVersion, Package: "test.module-url", Version: "1"}
	for _, name := range []string{"preview#hash.js", "preview%20literal.js", "preview%escape.js"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		manifest.Artifacts = append(manifest.Artifacts, distribution.Artifact{Path: name})
		manifest.Units = append(manifest.Units, distribution.Unit{ID: framehost.UnitID(name), Profile: framehost.BrowserJS, Module: &distribution.Module{Entry: name}})
	}
	var archive bytes.Buffer
	if _, err := distribution.Build(t.Context(), source, manifest, &archive); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(directory, "plugin.zip")
	if err := os.WriteFile(archivePath, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	release, err := store.ImportArchive(t.Context(), archivePath, value.Hash(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Content: store, preview: release}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _, err := app.artifact(r.Context(), strings.TrimPrefix(r.URL.Path, "/frame/artifacts/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	for _, unit := range manifest.Units {
		t.Run(string(unit.ID), func(t *testing.T) {
			module, err := (moduleSource{store: store}).Module(t.Context(), framehost.UnitBinding{Source: framehost.UnitRef{
				InstallationID: "preview", Release: release.Digest, UnitID: unit.ID,
			}})
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Get(server.URL + module.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != unit.Module.Entry {
				t.Fatalf("module URL changed the requested artifact: status=%d body=%q error=%v", response.StatusCode, body, err)
			}
		})
	}
}
