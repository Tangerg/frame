package distribution

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

// VerifyArchive validates a publisher artifact in disposable storage. It makes
// no publisher trust decision and never installs code into an application Host.
func VerifyArchive(ctx context.Context, archive string, digest value.Digest, limits Limits) (_ Release, err error) {
	directory, err := os.MkdirTemp("", "frame-verify-")
	if err != nil {
		return Release{}, err
	}
	defer func() { err = errors.Join(err, removeSealed(directory)) }()
	store, err := Open(Config{
		Directory: directory, Admission: frame.NewAdmission(), Limits: limits,
		Trust:    func(context.Context, Source, Manifest, Lock) error { return nil },
		Retained: func(context.Context, value.Digest) (bool, error) { return true, nil },
	})
	if err != nil {
		return Release{}, err
	}
	defer func() { err = errors.Join(err, store.Close(context.Background())) }()
	return store.ImportArchive(ctx, archive, digest)
}

func (s *Store) Inspect(ctx context.Context, digest value.Digest) (Release, error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return Release{}, err
	}
	defer done()
	return s.inspect(ctx, digest)
}

func (s *Store) inspect(ctx context.Context, digest value.Digest) (Release, error) {
	if digest.Validate() != nil {
		return Release{}, ErrInvalid
	}
	root, err := s.openRelease(digest)
	if err != nil {
		return Release{}, err
	}
	defer root.Close()
	release, err := s.validate(ctx, root)
	if err != nil {
		return Release{}, err
	}
	if release.Digest != digest {
		return Release{}, ErrIntegrity
	}
	return release, nil
}

func (s *Store) validate(ctx context.Context, root *os.Root) (Release, error) {
	release, err := s.readReleaseMetadata(ctx, root)
	if err != nil {
		return Release{}, err
	}
	verifier := artifactVerifier{ctx: ctx, root: root, limits: s.limits, expected: make(map[string]Artifact, len(release.Manifest.Artifacts)), paths: pathInventory{}}
	for _, artifact := range release.Manifest.Artifacts {
		verifier.expected[artifact.Path] = artifact
	}
	if err := fs.WalkDir(root.FS(), ".", verifier.visit); err != nil {
		return Release{}, err
	}
	if len(verifier.expected) != 0 {
		return Release{}, ErrIntegrity
	}
	return release, nil
}

func (s *Store) readReleaseMetadata(ctx context.Context, root *os.Root) (Release, error) {
	manifestFile, err := readRegular(ctx, root, ManifestName, s.limits.ManifestBytes)
	if err != nil {
		return Release{}, err
	}
	manifest, err := ParseManifest(manifestFile.data)
	if err != nil {
		return Release{}, err
	}
	digest, err := manifest.Identity()
	if err != nil {
		return Release{}, err
	}
	if !manifest.Release.IsZero() && manifest.Release != digest {
		return Release{}, ErrIntegrity
	}
	lockFile, err := readRegular(ctx, root, LockName, s.limits.ManifestBytes)
	if err != nil {
		return Release{}, err
	}
	lock, err := ParseLock(lockFile.data)
	if err != nil {
		return Release{}, err
	}
	if err := lock.Verify(manifest); err != nil {
		return Release{}, err
	}
	return Release{Digest: digest, Manifest: manifest, Lock: lock}, nil
}

type artifactVerifier struct {
	ctx      context.Context
	root     *os.Root
	limits   Limits
	expected map[string]Artifact
	paths    pathInventory
	size     int64
}

func (v *artifactVerifier) visit(name string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	if err := v.ctx.Err(); err != nil {
		return err
	}
	if name == "." {
		return nil
	}
	info, err := entry.Info()
	if err != nil {
		return err
	}
	kind := fileEntry
	if info.IsDir() {
		kind = directoryEntry
	}
	if err := v.paths.admit(name, kind); err != nil {
		return err
	}
	if len(v.paths) > v.limits.Entries {
		return ErrCapacity
	}
	if info.Mode()&specialFileModes != 0 {
		return ErrInvalid
	}
	if info.IsDir() {
		return nil
	}
	if err := v.account(info); err != nil {
		return err
	}
	if isReleaseMetadata(name) {
		return nil
	}
	artifact, ok := v.expected[name]
	if !ok {
		return ErrIntegrity
	}
	content, err := readRegular(v.ctx, v.root, name, artifact.Size)
	if err != nil {
		return err
	}
	if err := artifact.verify(content); err != nil {
		return err
	}
	delete(v.expected, name)
	return nil
}

func (v *artifactVerifier) account(info fs.FileInfo) error {
	if !info.Mode().IsRegular() {
		return ErrInvalid
	}
	if info.Size() > v.limits.ExpandedBytes-v.size {
		return ErrCapacity
	}
	v.size += info.Size()
	return nil
}

func (a Artifact) verify(content fileSnapshot) error {
	if int64(len(content.data)) != a.Size || value.Hash(content.data) != a.Digest || (content.mode.Perm()&fileExecuteMask != 0) != a.Executable {
		return ErrIntegrity
	}
	return nil
}
