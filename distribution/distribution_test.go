package distribution

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

func packageFixture(t *testing.T) (string, Manifest, []byte, Lock) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "theme.css"), []byte("body { color: blue; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{SchemaVersion: 1, Package: "test.theme", Version: "1.0.0", Artifacts: []Artifact{{Path: "theme.css"}}, Units: []Unit{{ID: "theme", Profile: "static-resource", Resources: []string{"theme.css"}}}}
	var archive bytes.Buffer
	lock, err := Build(context.Background(), directory, manifest, &archive)
	if err != nil {
		t.Fatal(err)
	}
	return directory, manifest, archive.Bytes(), lock
}

func storeFixture(t *testing.T, retained func(context.Context, framevalue.Digest) (bool, error)) *Store {
	t.Helper()
	if retained == nil {
		retained = func(context.Context, framevalue.Digest) (bool, error) { return false, nil }
	}
	store, err := Open(Config{Directory: t.TempDir(), Admission: frame.NewAdmission(), Trust: func(context.Context, Source, Manifest, Lock) error { return nil }, Retained: retained})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := removeSealed(store.directory); err != nil {
			t.Error(err)
		}
	})
	return store
}

func installArchive(t *testing.T, store *Store, data []byte) Release {
	t.Helper()
	file := filepath.Join(t.TempDir(), "plugin.zip")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := store.ImportArchive(context.Background(), file, framevalue.Hash(data))
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func TestBuildDeterministicAndResourceNeedsNoMethods(t *testing.T) {
	directory, manifest, archive, lock := packageFixture(t)
	var second bytes.Buffer
	other, err := Build(context.Background(), directory, manifest, &second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archive, second.Bytes()) || lock.Release != other.Release {
		t.Fatal("same release produced different bytes")
	}
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	if release.Digest != lock.Release || len(release.Manifest.Units[0].Exports) != 0 {
		t.Fatal("resource release changed its declaration")
	}
	lease, err := store.Acquire(context.Background(), release.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	data, err := lease.Read(context.Background(), "theme.css")
	if err != nil || string(data) != "body { color: blue; }\n" {
		t.Fatalf("resource: %q %v", data, err)
	}
}

func TestLockCannotIntroduceUnownedReleaseDependencies(t *testing.T) {
	_, _, archive, _ := packageFixture(t)
	release := installArchive(t, storeFixture(t, nil), archive)
	lock := release.Lock
	lock.Dependencies = []LockedRelease{{Package: "other.release", Release: framevalue.Hash([]byte("other"))}}
	if err := lock.Verify(release.Manifest); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("lock advanced dependencies without changing the manifest identity: %v", err)
	}
}

func TestDuplicateInstallReturnsAndTrustsTheFinalCachedSnapshot(t *testing.T) {
	source, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	first := installArchive(t, store, archive)
	manifest := first.Manifest
	manifest.Release = framevalue.Digest{}
	for name, value := range map[string]any{ManifestName: manifest, LockName: first.Lock} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var trusted Release
	store.trust = func(_ context.Context, _ Source, manifest Manifest, lock Lock) error {
		trusted = Release{Digest: lock.Release, Manifest: manifest, Lock: lock}
		return nil
	}
	lease, err := store.ImportDirectoryHeld(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	inspected, err := store.Inspect(t.Context(), first.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lease.Snapshot(), inspected) {
		t.Error("duplicate install returned a staged projection instead of the cached release")
	}
	if !reflect.DeepEqual(trusted, inspected) {
		t.Error("trust approved a staged projection instead of the final cached release")
	}
	again, err := store.Acquire(t.Context(), first.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !reflect.DeepEqual(again.Snapshot(), lease.Snapshot()) {
		t.Error("reacquiring the release changed its snapshot")
	}
	refusal := errors.New("cached projection refused")
	store.trust = func(_ context.Context, _ Source, manifest Manifest, _ Lock) error {
		if manifest.Release == first.Digest {
			return refusal
		}
		return nil
	}
	if _, err := store.ImportDirectoryHeld(t.Context(), source); !errors.Is(err, ErrUntrusted) || !errors.Is(err, refusal) {
		t.Fatalf("duplicate install bypassed final cache trust: %v", err)
	}
	if err := errors.Join(lease.Close(), again.Close()); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(t.Context(), first.Digest); err != nil {
		t.Fatalf("refused cached projection retained a leaked lease: %v", err)
	}
}

func TestManifestDependencyChangesCreateDistinctImmutableReleases(t *testing.T) {
	source, manifest, _, _ := packageFixture(t)
	store := storeFixture(t, nil)
	var releases []Release
	for _, name := range []string{"first", "second"} {
		manifest.Dependencies = []LockedRelease{{Package: "dependency", Release: framevalue.Hash([]byte(name))}}
		var archive bytes.Buffer
		lock, err := Build(t.Context(), source, manifest, &archive)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(lock.Dependencies, manifest.Dependencies) {
			t.Fatal("lock does not project its manifest dependencies")
		}
		releases = append(releases, installArchive(t, store, archive.Bytes()))
	}
	if releases[0].Digest == releases[1].Digest {
		t.Fatal("changed dependency retained the same release identity")
	}
	for _, release := range releases {
		inspected, err := store.Inspect(t.Context(), release.Digest)
		if err != nil || !reflect.DeepEqual(release, inspected) {
			t.Fatalf("release changed in cache: %+v, %v", inspected, err)
		}
	}
	for _, invalid := range [][]LockedRelease{
		{{Package: "invalid package", Release: framevalue.Hash([]byte("valid"))}},
		{{Package: "dependency", Release: framevalue.Digest{}}},
		{manifest.Dependencies[0], manifest.Dependencies[0]},
	} {
		manifest.Dependencies = invalid
		if _, err := manifest.Identity(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid canonical dependencies admitted: %+v, %v", invalid, err)
		}
	}
}

func TestArchivePathsAliasesAndSymlinksAreRefusedBeforeTrust(t *testing.T) {
	_, _, valid, _ := packageFixture(t)
	for _, name := range []string{"../escape", "/absolute", "a\\b", "A/theme.css", "con.txt"} {
		t.Run(name, func(t *testing.T) {
			store := storeFixture(t, nil)
			data := rewriteArchive(t, valid, func(writer *zip.Writer) {
				file, err := writer.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = file.Write([]byte("bad"))
			})
			file := filepath.Join(t.TempDir(), "bad.zip")
			_ = os.WriteFile(file, data, 0600)
			if _, err := store.ImportArchive(context.Background(), file, framevalue.Hash(data)); err == nil {
				t.Fatal("unsafe or undeclared archive was accepted")
			}
		})
	}
	store := storeFixture(t, nil)
	data := rewriteArchive(t, valid, func(writer *zip.Writer) {
		header := &zip.FileHeader{Name: "link"}
		header.SetMode(os.ModeSymlink | 0777)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte("/tmp/outside"))
	})
	file := filepath.Join(t.TempDir(), "symlink.zip")
	_ = os.WriteFile(file, data, 0600)
	if _, err := store.ImportArchive(context.Background(), file, framevalue.Hash(data)); err == nil {
		t.Fatal("symlink archive accepted")
	}
}

func TestArchiveDigestLimitsAndTrust(t *testing.T) {
	_, _, data, _ := packageFixture(t)
	store := storeFixture(t, nil)
	file := filepath.Join(t.TempDir(), "plugin.zip")
	_ = os.WriteFile(file, data, 0600)
	if _, err := store.ImportArchive(context.Background(), file, framevalue.Hash([]byte("different"))); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("digest: %v", err)
	}
	store.limits.ExpandedBytes = 4
	if _, err := store.ImportArchive(context.Background(), file, framevalue.Hash(data)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("size: %v", err)
	}
	store.limits = DefaultLimits()
	store.trust = func(context.Context, Source, Manifest, Lock) error { return errors.New("publisher is not approved") }
	if _, err := store.ImportArchive(context.Background(), file, framevalue.Hash(data)); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("trust: %v", err)
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected staging remained: %v %v", entries, err)
	}
}

func TestDirectorySnapshotCannotFollowLaterSourceChanges(t *testing.T) {
	_, _, data, _ := packageFixture(t)
	root := t.TempDir()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range archive.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, entry.Name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := storeFixture(t, nil)
	release, err := store.ImportDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "theme.css"), []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(context.Background(), release.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	actual, err := lease.Read(context.Background(), "theme.css")
	if err != nil || string(actual) != "body { color: blue; }\n" {
		t.Fatalf("source replaced release: %q %v", actual, err)
	}
	if _, err := store.ImportDirectory(context.Background(), root); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("drift source accepted: %v", err)
	}
}

func TestReadAndDurableProtectionPreventCollection(t *testing.T) {
	_, _, data, _ := packageFixture(t)
	retained := true
	store := storeFixture(t, func(context.Context, framevalue.Digest) (bool, error) { return retained, nil })
	release := installArchive(t, store, data)
	lease, err := store.Acquire(context.Background(), release.Digest)
	if err != nil {
		t.Fatal(err)
	}
	retained = false
	if err := store.Remove(context.Background(), release.Digest); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("live read removed: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	retained = true
	if err := store.Remove(context.Background(), release.Digest); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("durable protection removed: %v", err)
	}
	retained = false
	if err := store.Remove(context.Background(), release.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Read(context.Background(), "theme.css"); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("closed lease read: %v", err)
	}
}

func TestCachedByteDriftAndManifestAliasesAreRefused(t *testing.T) {
	_, manifest, data, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, data)
	file := filepath.Join(store.directory, release.Digest.String(), "theme.css")
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("changed cache"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(context.Background(), release.Digest); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("cache drift: %v", err)
	}
	manifest.Artifacts = []Artifact{{Path: "A/one", Digest: framevalue.Hash(nil)}, {Path: "a/two", Digest: framevalue.Hash(nil)}}
	manifest.Units[0].Resources = nil
	if err := manifest.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("parent alias: %v", err)
	}
}

func TestManifestDuplicateKeysAndWrongLockAreRefused(t *testing.T) {
	if _, err := ParseManifest([]byte(`{"schemaVersion":1,"schemaVersion":1,"package":"p","version":"1","artifacts":[],"units":[]}`)); err == nil {
		t.Fatal("duplicate member accepted")
	}
	_, _, data, _ := packageFixture(t)
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	var lock Lock
	for _, entry := range archive.File {
		reader, _ := entry.Open()
		body, _ := io.ReadAll(reader)
		_ = reader.Close()
		if entry.Name == ManifestName {
			manifest, err = ParseManifest(body)
		}
		if entry.Name == LockName {
			lock, err = ParseLock(body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	lock.Release = framevalue.Hash([]byte("another release"))
	if err := lock.Verify(manifest); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong lock: %v", err)
	}
	body, _ := json.Marshal(manifest)
	if _, err := ParseManifest(body); err != nil {
		t.Fatal(err)
	}
}

func rewriteArchive(t *testing.T, data []byte, appendFiles func(*zip.Writer)) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	writer := zip.NewWriter(&result)
	for _, entry := range reader.File {
		source, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		header := entry.FileHeader
		target, err := writer.CreateHeader(&header)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(target, source)
		_ = source.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	appendFiles(writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func TestPublicationSealsTheTreeAndRefusesAnIncompleteRoot(t *testing.T) {
	_, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	directory := filepath.Join(store.directory, release.Digest.String())
	if err := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0222 != 0 {
			t.Errorf("published path remains writable: %s", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(t.Context(), release.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Read(t.Context(), "theme.css"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unsealed root remained readable: %v", err)
	}
	if _, err := store.Acquire(t.Context(), release.Digest); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unsealed root accepted a new lease: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(t.Context(), release.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete publication was not removed: %v", err)
	}
}
