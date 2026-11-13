//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package sqlite

import (
	"os"
	"path/filepath"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

func TestDatabaseAuthoritiesInOneDirectoryRemainIndependent(t *testing.T) {
	directory := t.TempDir()
	first, err := Open(t.Context(), testConfig(filepath.Join(directory, "first.db"), framehost.NewAdmission()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	})
	second, err := Open(t.Context(), testConfig(filepath.Join(directory, "second.db"), framehost.NewAdmission()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := first.Installations(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Installations(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseAuthorityRejectsHardLinkAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(path), "alias.db")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, alias} {
		if store, err := Open(t.Context(), testConfig(name, framehost.NewAdmission())); err == nil {
			_ = store.Close()
			t.Fatal("database authority accepted a hard-link alias")
		}
	}
}
