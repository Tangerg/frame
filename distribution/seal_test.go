package distribution

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestNestedReleaseRetainsItsFinalSealWhenTheStoreReopens(t *testing.T) {
	files := []struct {
		path       string
		content    []byte
		executable bool
	}{
		{path: "assets/theme.css", content: []byte("body { color: blue; }\n")},
		{path: "bin/tools/render", content: []byte("#!/bin/sh\nexit 0\n"), executable: true},
	}
	source := t.TempDir()
	manifest := Manifest{SchemaVersion: 1, Package: "test.theme", Version: "1.0.0"}
	unit := Unit{ID: "theme", Profile: "static-resource"}
	for _, file := range files {
		path := filepath.Join(source, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(path), privateDirectoryMode); err != nil {
			t.Fatal(err)
		}
		artifact := Artifact{Path: file.path, Executable: file.executable}
		if err := os.WriteFile(path, file.content, artifact.fileMode(unsealedTree)); err != nil {
			t.Fatal(err)
		}
		manifest.Artifacts = append(manifest.Artifacts, artifact)
		unit.Resources = append(unit.Resources, file.path)
	}
	manifest.Units = []Unit{unit}
	var archive bytes.Buffer
	if _, err := Build(t.Context(), source, manifest, &archive); err != nil {
		t.Fatal(err)
	}
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive.Bytes())
	releasePath := filepath.Join(store.directory, release.Digest.String())
	original, err := os.Stat(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Config{Directory: store.directory, Admission: frame.NewAdmission(), Trust: store.trust, Retained: store.retained})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	lease, err := reopened.Acquire(t.Context(), release.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	for _, file := range files {
		content, err := lease.Read(t.Context(), file.path)
		if err != nil || !bytes.Equal(content, file.content) {
			t.Fatalf("reopened artifact %s changed: %q, %v", file.path, content, err)
		}
	}
	err = filepath.WalkDir(releasePath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := sealedFileMode
		if entry.IsDir() {
			mode = sealedDirectoryMode
		} else if path == filepath.Join(releasePath, "bin", "tools", "render") {
			mode |= ownerExecuteBit
		}
		if info.Mode().Perm() != mode {
			t.Errorf("unsealed publication %s: mode=%o, want %o", path, info.Mode().Perm(), mode)
		}
		if path == releasePath && !os.SameFile(original, info) {
			t.Error("reopening replaced the original release directory")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCachedArtifactDriftRequiresExplicitPublicationRepair(t *testing.T) {
	_, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	path := filepath.Join(store.directory, release.Digest.String(), "theme.css")
	if err := os.Chmod(path, privateFileMode); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(t.Context(), release.Digest)
	if lease != nil {
		_ = lease.Close()
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("writable cached artifact was admitted: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != privateFileMode {
		t.Fatalf("acquire changed the cached artifact: %v", err)
	}
	file := filepath.Join(t.TempDir(), "original.zip")
	if err := os.WriteFile(file, archive, privateFileMode); err != nil {
		t.Fatal(err)
	}
	_, importErr := store.ImportArchive(t.Context(), file, value.Hash(archive))
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	t.Logf("reimport error=%v; final artifact mode=%o", importErr, info.Mode().Perm())
	if importErr != nil || info.Mode().Perm() != sealedFileMode {
		t.Fatal("successful cached import returned a mutable artifact as a sealed release")
	}
}
