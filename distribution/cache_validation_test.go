package distribution

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCachedTreeRevalidatesPathsAndCapacity(t *testing.T) {
	for _, mutation := range []struct {
		name   string
		change func(*testing.T, string)
		want   error
	}{
		{name: "unportable directory", want: ErrInvalid, change: func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "con"), sealedDirectoryMode); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra directory capacity", want: ErrCapacity, change: func(t *testing.T, root string) {
			for _, name := range []string{"one", "two"} {
				if err := os.Mkdir(filepath.Join(root, name), sealedDirectoryMode); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{name: "special file mode", want: ErrInvalid, change: func(t *testing.T, root string) {
			file := filepath.Join(root, "theme.css")
			if err := os.Chmod(file, sealedFileMode|fs.ModeSetuid); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(file)
			if err != nil || info.Mode()&fs.ModeSetuid == 0 {
				t.Fatalf("fixture did not acquire special mode: %v", err)
			}
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			_, _, archive, _ := packageFixture(t)
			store := storeFixture(t, nil)
			store.limits.Entries = 4
			release := installArchive(t, store, archive)
			root := filepath.Join(store.directory, release.Digest.String())
			if err := os.Chmod(root, privateDirectoryMode); err != nil {
				t.Fatal(err)
			}
			mutation.change(t, root)
			if err := os.Chmod(root, sealedDirectoryMode); err != nil {
				t.Fatal(err)
			}
			lease, err := store.Acquire(t.Context(), release.Digest)
			if lease != nil {
				_ = lease.Close()
			}
			if !errors.Is(err, mutation.want) {
				t.Fatalf("cached tree bypassed import policy: got %v, want %v", err, mutation.want)
			}
		})
	}
}

func TestLeaseReadsRevalidateArtifactExecutability(t *testing.T) {
	for _, test := range []struct {
		name       string
		executable bool
	}{{name: "read"}, {name: "executable", executable: true}} {
		t.Run(test.name, func(t *testing.T) {
			source, manifest, _, _ := packageFixture(t)
			manifest.Artifacts[0].Executable = test.executable
			var archive bytes.Buffer
			if _, err := Build(t.Context(), source, manifest, &archive); err != nil {
				t.Fatal(err)
			}
			store := storeFixture(t, nil)
			release := installArchive(t, store, archive.Bytes())
			lease, err := store.Acquire(t.Context(), release.Digest)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			mode := sealedFileMode
			if !test.executable {
				mode |= ownerExecuteBit
			}
			if err := os.Chmod(filepath.Join(store.directory, release.Digest.String(), "theme.css"), mode); err != nil {
				t.Fatal(err)
			}
			if test.executable {
				_, err = lease.Executable(t.Context(), "theme.css")
			} else {
				_, err = lease.Read(t.Context(), "theme.css")
			}
			if !errors.Is(err, ErrIntegrity) {
				t.Fatalf("leased artifact executability drift was accepted: %v", err)
			}
		})
	}
}
