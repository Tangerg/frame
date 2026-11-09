package distribution

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/value"
)

func TestExecutableInventoryOwnsPublishedPermissions(t *testing.T) {
	for _, executable := range []bool{false, true} {
		for _, mode := range []fs.FileMode{0, 0o010, 0o001, 0o100, 0o111} {
			t.Run(fmt.Sprintf("executable=%t/mode=%03o", executable, mode), func(t *testing.T) {
				source, manifest, _, _ := packageFixture(t)
				if err := os.WriteFile(filepath.Join(source, "theme.css"), []byte("#!/bin/sh\nexit 0\n"), privateFileMode); err != nil {
					t.Fatal(err)
				}
				manifest.Artifacts[0].Executable = executable
				var built bytes.Buffer
				if _, err := Build(t.Context(), source, manifest, &built); err != nil {
					t.Fatal(err)
				}
				reader, err := zip.NewReader(bytes.NewReader(built.Bytes()), int64(built.Len()))
				if err != nil {
					t.Fatal(err)
				}
				var changed bytes.Buffer
				writer := zip.NewWriter(&changed)
				for _, entry := range reader.File {
					header := entry.FileHeader
					if header.Name == "theme.css" {
						header.SetMode(mode)
					}
					output, err := writer.CreateHeader(&header)
					if err != nil {
						t.Fatal(err)
					}
					input, err := entry.Open()
					if err != nil {
						t.Fatal(err)
					}
					_, copyErr := io.Copy(output, input)
					if err := errors.Join(copyErr, input.Close()); err != nil {
						t.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				store := storeFixture(t, nil)
				archive := filepath.Join(t.TempDir(), "release.zip")
				if err := os.WriteFile(archive, changed.Bytes(), privateFileMode); err != nil {
					t.Fatal(err)
				}
				release, err := store.ImportArchive(t.Context(), archive, value.Hash(changed.Bytes()))
				if (mode&fileExecuteMask != 0) != executable {
					if !errors.Is(err, ErrIntegrity) {
						t.Fatalf("mismatched executable declaration admitted: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				lease, err := store.Acquire(t.Context(), release.Digest)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
				path := filepath.Join(store.directory, release.Digest.String(), "theme.css")
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				publishedMode := sealedFileMode
				if executable {
					publishedMode |= ownerExecuteBit
				}
				if info.Mode().Perm() != publishedMode {
					t.Fatalf("published mode=%o", info.Mode().Perm())
				}
				path, err = lease.Executable(t.Context(), "theme.css")
				if !executable {
					if !errors.Is(err, ErrInvalid) {
						t.Fatalf("non-executable exposed: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := exec.CommandContext(t.Context(), path).Run(); err != nil {
					t.Fatalf("verified executable could not execute: %v", err)
				}
			})
		}
	}
}
