package localfile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/internal/localfile"
)

func TestGeneratedFileReplacesTheDirectoryEntry(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "generated.go")
	if err := localfile.WriteGenerated(path, []byte("original source")); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(directory, "previous.go")
	if err := os.Link(path, previous); err != nil {
		t.Fatal(err)
	}
	if err := localfile.WriteGenerated(path, []byte("replacement source")); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		path string
		want string
	}{{path, "replacement source"}, {previous, "original source"}} {
		data, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != file.want {
			t.Fatalf("%s = %q, want %q", file.path, data, file.want)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != localfile.PublicFileMode {
		t.Fatalf("generated file mode = %v, want %v", info.Mode().Perm(), localfile.PublicFileMode)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("publication left unexpected entries: %v", entries)
	}
}

func TestGeneratedFileRejectsNonregularTargets(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			original := filepath.Join(directory, "original.go")
			if err := os.WriteFile(original, []byte("original source"), localfile.PublicFileMode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "generated.go")
			if kind == "directory" {
				if err := os.Mkdir(path, localfile.PublicDirectoryMode); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(original, path); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := localfile.WriteGenerated(path, []byte("replacement source")); !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("nonregular target rejection = %v, want invalid", err)
			}
			after, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Fatal("rejected destination was replaced")
			}
			data, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "original source" {
				t.Fatalf("rejected publication changed original source: %q", data)
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("rejected publication left unexpected entries: %v", entries)
			}
		})
	}
}

func TestGeneratedFilePreservesStagingFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "missing", "generated.go")
	if err := localfile.WriteGenerated(path, []byte("source")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging error = %v, want not exist", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed staging created unexpected entries: %v", entries)
	}
}
