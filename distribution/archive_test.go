package distribution

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBoundedArchiveDirectory(t *testing.T) {
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, name := range []string{"one", "two", "three"} {
		if _, err := writer.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.Entries = 2
	if _, err := limits.openArchive(data.Bytes()); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	// A small count in the end record must not hide additional physical entries.
	const endBytes = 22
	end := data.Bytes()[data.Len()-endBytes:]
	binary.LittleEndian.PutUint16(end[8:], 1)
	binary.LittleEndian.PutUint16(end[10:], 1)
	if _, err := limits.openArchive(data.Bytes()); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(end[8:], 0xffff)
	binary.LittleEndian.PutUint16(end[10:], 0xffff)
	if _, err := limits.openArchive(data.Bytes()); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestCompressionRatioIncludesRemainder(t *testing.T) {
	limits := DefaultLimits()
	limits.CompressionRatio = 2
	for _, item := range []struct {
		expanded, compressed uint64
		allowed              bool
	}{{199, 100, true}, {200, 100, true}, {201, 100, false}, {0, 0, true}, {1, 0, false}} {
		if got := limits.admitsExpansion(item.expanded, item.compressed); got != item.allowed {
			t.Fatalf("%d/%d: admitted %t", item.expanded, item.compressed, got)
		}
	}
}

func TestBuildRejectsReservedNamespaceBeforePublication(t *testing.T) {
	for _, name := range []string{"MANIFEST.JSON", "Plugin.Lock.Json", "manifest.json/entry"} {
		dir := t.TempDir()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("artifact"), 0600); err != nil {
			t.Fatal(err)
		}
		manifest := Manifest{SchemaVersion: ManifestSchemaVersion, Package: "demo", Version: "1", Artifacts: []Artifact{{Path: name}}}
		var out bytes.Buffer
		if _, err := Build(t.Context(), dir, manifest, &out); !errors.Is(err, ErrInvalid) || out.Len() != 0 {
			t.Fatalf("%q: %v, %d bytes", name, err, out.Len())
		}
	}
}

func TestReimportFinishesInterruptedSeal(t *testing.T) {
	_, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	tree := filepath.Join(store.directory, release.Digest.String())
	if err := os.Chmod(tree, 0700); err != nil {
		t.Fatal(err)
	}
	restored := installArchive(t, store, archive)
	if restored.Digest != release.Digest {
		t.Fatal("release changed on reimport")
	}
	if _, err := store.Inspect(t.Context(), release.Digest); err != nil {
		t.Fatal(err)
	}
}
