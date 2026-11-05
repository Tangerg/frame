package distribution

import (
	"archive/zip"
	"bytes"
	"errors"
	"testing"
)

func archiveSeed(f *testing.F, names ...string) []byte {
	f.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			f.Fatal(err)
		}
		if _, err = entry.Write([]byte(name)); err != nil {
			f.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		f.Fatal(err)
	}
	return data.Bytes()
}

func FuzzOpenArchiveRefusesWithAClass(f *testing.F) {
	limits := DefaultLimits()
	limits.Entries = 4
	for _, archive := range [][]byte{
		archiveSeed(f),
		archiveSeed(f, "manifest.json"),
		archiveSeed(f, "manifest.json", "unit/index.js", "unit/README.md"),
		archiveSeed(f, "a", "b", "c", "d", "e"),
	} {
		f.Add(archive)
		f.Add(archive[:len(archive)-1])
		f.Add(archive[len(archive)/2:])
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		reader, err := limits.openArchive(data)
		if err != nil {
			if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrCapacity) {
				t.Fatalf("unclassified archive refusal: %v", err)
			}
			return
		}
		if len(reader.File) > limits.Entries {
			t.Fatalf("admitted %d entries under a limit of %d", len(reader.File), limits.Entries)
		}
	})
}
