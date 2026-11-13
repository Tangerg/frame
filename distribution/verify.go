package distribution

import (
	"context"
	"errors"
	"io/fs"
	"os"

	framehost "github.com/Tangerg/frame/host"
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
		Directory: directory, Admission: framehost.NewAdmission(), Limits: limits,
		Trust:    func(context.Context, Source, Manifest, Lock) error { return nil },
		Retained: func(context.Context, value.Digest) (bool, error) { return true, nil },
	})
	if err != nil {
		return Release{}, err
	}
	defer func() { err = errors.Join(err, store.Close(context.Background())) }()
	return store.ImportArchive(ctx, archive, digest)
}

func (s *storeAuthority) Inspect(ctx context.Context, digest value.Digest) (Release, error) {
	done, err := s.beginOperation(newOperation)
	if err != nil {
		return Release{}, err
	}
	defer done()
	return s.inspect(ctx, digest)
}

func (s *storeAuthority) inspect(ctx context.Context, digest value.Digest) (Release, error) {
	if digest.Validate() != nil {
		return Release{}, ErrInvalid
	}
	root, err := s.openRelease(digest)
	if err != nil {
		return Release{}, err
	}
	defer root.Close()
	release, err := s.validate(ctx, root, sealedTree)
	if err != nil {
		return Release{}, err
	}
	if release.Digest != digest {
		return Release{}, ErrIntegrity
	}
	return release, nil
}

func (s *storeAuthority) validate(ctx context.Context, root *os.Root, admission treeAdmission) (Release, error) {
	release, err := s.readReleaseMetadata(ctx, root)
	if err != nil {
		return Release{}, err
	}
	verifier := artifactVerifier{ctx: ctx, root: root, admission: admission, limits: s.limits, expected: make(map[string]Artifact, len(release.Manifest.Artifacts))}
	for _, artifact := range release.Manifest.Artifacts {
		verifier.expected[artifact.Path] = artifact
	}
	paths := pathInventory{}
	if err := paths.walk(ctx, root, s.limits.Entries, verifier.visit); err != nil {
		return Release{}, err
	}
	if len(verifier.expected) != 0 {
		return Release{}, ErrIntegrity
	}
	return release, nil
}

func (s *storeAuthority) readReleaseMetadata(ctx context.Context, root *os.Root) (Release, error) {
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
	admission treeAdmission
	ctx       context.Context
	root      *os.Root
	limits    Limits
	expected  map[string]Artifact
	size      int64
}

func (v *artifactVerifier) visit(name string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if info.Mode()&specialFileModes != 0 {
		return ErrInvalid
	}
	if info.IsDir() {
		if v.admission == sealedTree && info.Mode().Perm() != sealedDirectoryMode {
			return ErrIntegrity
		}
		return nil
	}
	if err := v.account(info); err != nil {
		return err
	}
	if isReleaseMetadata(name) {
		if v.admission == sealedTree && info.Mode().Perm() != sealedFileMode {
			return ErrIntegrity
		}
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
	if err := artifact.verify(content, v.admission); err != nil {
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

func (a Artifact) verify(content fileSnapshot, admission treeAdmission) error {
	if admission == sealedTree && content.mode.Perm() != a.fileMode(sealedTree) {
		return ErrIntegrity
	}
	if int64(len(content.data)) != a.Size || value.Hash(content.data) != a.Digest || (content.mode.Perm()&fileExecuteMask != 0) != a.Executable {
		return ErrIntegrity
	}
	return nil
}
