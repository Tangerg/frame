package distribution

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Tangerg/frame/value"
)

// Build computes the artifact inventory from the declared source files, then
// writes a deterministic ZIP and its exact lock. It does not execute build tools
// or include undeclared source files. The source remains mutable; its output is not.
func Build(ctx context.Context, source string, manifest Manifest, output io.Writer) (Lock, error) {
	input, err := os.OpenRoot(source)
	if err != nil {
		return Lock{}, err
	}
	defer input.Close()
	manifest.Artifacts = slices.Clone(manifest.Artifacts)
	slices.SortFunc(manifest.Artifacts, func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
	limits := DefaultLimits()
	paths := releasePaths()
	if len(manifest.Artifacts)+len(paths) > limits.Files {
		return Lock{}, ErrCapacity
	}
	files := make(map[string][]byte, len(manifest.Artifacts)+len(paths))
	var total int64
	for i, artifact := range manifest.Artifacts {
		if err := paths.admit(artifact.Path, false); err != nil {
			return Lock{}, err
		}
		if len(paths) > limits.Files {
			return Lock{}, ErrCapacity
		}
		data, err := readRegular(ctx, input, artifact.Path, limits.ExpandedBytes-total)
		if err != nil {
			return Lock{}, err
		}
		total += int64(len(data))
		manifest.Artifacts[i].Digest = value.Hash(data)
		manifest.Artifacts[i].Size = int64(len(data))
		files[artifact.Path] = data
	}
	manifest.Release = value.Digest{}
	lock, err := manifest.Lock()
	if err != nil {
		return Lock{}, err
	}
	manifest.Release = lock.Release
	files[ManifestName], err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Lock{}, err
	}
	files[LockName], err = json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return Lock{}, err
	}
	for _, name := range []string{ManifestName, LockName} {
		size := int64(len(files[name]))
		if size > limits.ManifestBytes || size > limits.ExpandedBytes-total {
			return Lock{}, ErrCapacity
		}
		total += size
	}
	archive := zip.NewWriter(&limitedWriter{writer: output, remaining: limits.ArchiveBytes})
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Lock{}, errors.Join(err, archive.Close())
		}
		// Stored entries avoid a package tool producing an archive that its own
		// expansion-ratio policy would reject for highly compressible resources.
		header := &zip.FileHeader{Name: name, Method: zip.Store, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		mode := privateFileMode
		for _, artifact := range manifest.Artifacts {
			if artifact.Path == name && artifact.Executable {
				mode |= ownerExecuteBit
				break
			}
		}
		header.SetMode(mode)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return Lock{}, errors.Join(err, archive.Close())
		}
		if _, err := writer.Write(files[name]); err != nil {
			return Lock{}, errors.Join(err, archive.Close())
		}
	}
	return lock, archive.Close()
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, ErrCapacity
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}
