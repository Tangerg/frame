package distribution

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/value"
)

// Author sources remain mutable, so the inventory and deterministic archive
// must derive from the same captured bytes.
func Build(ctx context.Context, source string, manifest Manifest, output io.Writer) (Lock, error) {
	input, err := os.OpenRoot(source)
	if err != nil {
		return Lock{}, err
	}
	defer input.Close()
	limits := DefaultLimits()
	manifest.Artifacts = slices.SortedFunc(slices.Values(manifest.Artifacts), func(a, b Artifact) int { return strings.Compare(a.Path, b.Path) })
	contents := releaseContents{files: make(map[string][]byte, len(manifest.Artifacts)+2), limits: limits}
	if err := contents.readArtifacts(ctx, input, manifest.Artifacts); err != nil {
		return Lock{}, err
	}
	manifest.Release = value.Digest{}
	lock, err := manifest.Lock()
	if err != nil {
		return Lock{}, err
	}
	manifest.Release = lock.Release
	if err := contents.addMetadata(ManifestName, manifest); err != nil {
		return Lock{}, err
	}
	if err := contents.addMetadata(LockName, lock); err != nil {
		return Lock{}, err
	}
	return lock, contents.writeArchive(ctx, output, manifest.Artifacts)
}

type releaseContents struct {
	files  map[string][]byte
	total  int64
	limits Limits
}

func (c *releaseContents) readArtifacts(ctx context.Context, input *os.Root, artifacts []Artifact) error {
	paths := reservedMetadataPaths()
	if len(artifacts)+len(paths) > c.limits.Entries {
		return ErrCapacity
	}
	for i, artifact := range artifacts {
		if err := paths.admitBounded(artifact.Path, fileEntry, c.limits.Entries); err != nil {
			return err
		}
		content, err := readRegular(ctx, input, artifact.Path, c.limits.ExpandedBytes-c.total)
		if err != nil {
			return err
		}
		c.total += int64(len(content.data))
		artifacts[i].Digest = value.Hash(content.data)
		artifacts[i].Size = int64(len(content.data))
		c.files[artifact.Path] = content.data
	}
	return nil
}

func (c *releaseContents) addMetadata(name string, document any) error {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	size := int64(len(data))
	if size > c.limits.ManifestBytes || size > c.limits.ExpandedBytes-c.total {
		return ErrCapacity
	}
	c.total += size
	c.files[name] = data
	return nil
}

// zipEpoch is the earliest time a ZIP entry can record. Every entry uses it,
// so equal contents always produce equal archive bytes.
var zipEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

func (c *releaseContents) writeArchive(ctx context.Context, output io.Writer, artifacts []Artifact) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, cancellation.Err(ctx))
		}
	}()
	archive := zip.NewWriter(&limitedWriter{writer: output, remaining: c.limits.ArchiveBytes})
	for _, name := range slices.Sorted(maps.Keys(c.files)) {
		if err := cancellation.Err(ctx); err != nil {
			return errors.Join(err, archive.Close())
		}
		// Stored entries avoid a package tool producing an archive that its own
		// expansion-ratio policy would reject for highly compressible resources.
		header := &zip.FileHeader{Name: name, Method: zip.Store, Modified: zipEpoch}
		mode := privateFileMode
		if slices.ContainsFunc(artifacts, func(artifact Artifact) bool { return artifact.Path == name && artifact.Executable }) {
			mode |= ownerExecuteBit
		}
		header.SetMode(mode)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return errors.Join(err, archive.Close())
		}
		if _, err := writer.Write(c.files[name]); err != nil {
			return errors.Join(err, archive.Close())
		}
	}
	return archive.Close()
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
