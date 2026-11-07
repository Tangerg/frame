package distribution

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type countedDirectory struct {
	fs.ReadDirFile
	requests []int
	entries  int
}

func (d *countedDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	d.requests = append(d.requests, n)
	entries, err := d.ReadDirFile.ReadDir(n)
	d.entries += len(entries)
	return entries, err
}

func TestDirectoryEnumerationUsesTheOriginalEntryBudget(t *testing.T) {
	source := t.TempDir()
	for index := range 2 * directoryReadBatch {
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("entry-%04d", index)), nil, privateFileMode); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, limit := range []int{1, 2, directoryReadBatch - 1, directoryReadBatch + 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			file, err := root.Open(".")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			directory := &countedDirectory{ReadDirFile: file}
			paths := reservedMetadataPaths()
			_, err = paths.readEntries(t.Context(), directory, ".", len(paths)+limit)
			if !errors.Is(err, ErrCapacity) {
				t.Fatalf("oversized directory was admitted: %v", err)
			}
			if directory.entries != limit+1 {
				t.Fatalf("allocated %d entries for %d available slots and one excess probe", directory.entries, limit)
			}
			for _, requested := range directory.requests {
				if requested < 1 || requested > min(directoryReadBatch, limit) {
					t.Fatalf("unbounded directory read: %v", directory.requests)
				}
			}
		})
	}
}

func TestDirectoryWalkCountsEveryDirectoryAndFileOnce(t *testing.T) {
	source := t.TempDir()
	for _, name := range []string{"branch/inner/file", "branch/sibling", "leaf"} {
		filename := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(filename), privateDirectoryMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, nil, privateFileMode); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	expected := []string{"branch", "branch/inner", "branch/inner/file", "branch/sibling", "leaf"}
	for _, limit := range []int{len(expected) - 1, len(expected)} {
		paths := pathInventory{}
		var visited []string
		err := paths.walk(t.Context(), root, limit, func(name string, _ fs.DirEntry) error {
			visited = append(visited, name)
			return nil
		})
		if limit < len(expected) {
			if !errors.Is(err, ErrCapacity) {
				t.Fatalf("nested directory bypassed shared capacity: %v", err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(visited, expected) || len(paths) != len(expected) {
			t.Fatalf("admitted tree changed: paths=%v visited=%v error=%v", paths, visited, err)
		}
	}
}

func TestDirectoryWalkRefusesReplacementBeforeDescending(t *testing.T) {
	source := t.TempDir()
	nested := filepath.Join(source, "nested")
	if err := os.Mkdir(nested, privateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	paths := pathInventory{}
	err = paths.walk(t.Context(), root, DefaultLimits().Entries, func(name string, _ fs.DirEntry) error {
		if name != "nested" {
			t.Fatalf("replacement entered the original inventory: %s", name)
		}
		if err := os.Rename(nested, filepath.Join(source, "original")); err != nil {
			return err
		}
		return os.Mkdir(nested, privateDirectoryMode)
	})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("replacement directory was followed: %v", err)
	}
}
