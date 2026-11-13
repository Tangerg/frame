package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/internal/localfile"
)

func projectionDirectory(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{"sdk/src", "process"} {
		if err := os.MkdirAll(filepath.Join(root, path), localfile.PublicDirectoryMode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGenerationPreservesPreviousHardLinks(t *testing.T) {
	root := projectionDirectory(t)
	previous := filepath.Join(root, "previous.ts")
	if err := os.WriteFile(previous, []byte("previous protocol"), localfile.PublicFileMode); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "sdk/src/protocol.generated.ts")
	if err := os.Link(previous, target); err != nil {
		t.Fatal(err)
	}
	if err := generate(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(previous)
	if err != nil || string(data) != "previous protocol" {
		t.Fatalf("generation changed the previous file through its hard link: %v", err)
	}
	data, err = os.ReadFile(target)
	if err != nil || string(data) == "previous protocol" {
		t.Fatalf("generation did not publish the new protocol: %v", err)
	}
}

func TestGenerationRejectsSymlinkTargets(t *testing.T) {
	root := projectionDirectory(t)
	previous := filepath.Join(root, "unrelated.ts")
	if err := os.WriteFile(previous, []byte("unrelated source"), localfile.PublicFileMode); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "sdk/src/protocol.generated.ts")
	if err := os.Symlink(previous, target); err != nil {
		t.Fatal(err)
	}
	if err := generate(root); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("generation admitted a symlink target: %v", err)
	}
	data, err := os.ReadFile(previous)
	if err != nil || string(data) != "unrelated source" {
		t.Fatalf("generation changed unrelated source: %q, %v", data, err)
	}
}
