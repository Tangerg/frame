package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/value"
)

type Limits struct {
	Files            int
	ExpandedBytes    int64
	ArchiveBytes     int64
	ManifestBytes    int64
	CompressionRatio uint64
}

func DefaultLimits() Limits {
	return Limits{Files: 10000, ExpandedBytes: 512 << 20, ArchiveBytes: 256 << 20, ManifestBytes: 4 << 20, CompressionRatio: 200}
}

type Source struct {
	Location      string
	ArchiveDigest value.Digest
}

// Trust is supplied by the application. A digest fixes bytes but cannot decide
// whether their author, source or requested capabilities are trusted.
type Trust func(context.Context, Source, Manifest, Lock) error

type Config struct {
	Directory string
	Admission *frame.Admission
	Limits    Limits
	Trust     Trust
	Retained  func(context.Context, value.Digest) (bool, error)
}

type Store struct {
	directory      string
	root           *os.Root
	identity       fs.FileInfo
	lock           *os.File
	admission      *frame.Admission
	limits         Limits
	trust          Trust
	retained       func(context.Context, value.Digest) (bool, error)
	mu             sync.Mutex
	leases         map[value.Digest]int
	operations     int
	closing        bool
	closeAttempted bool
	closeDone      chan struct{}
	closeErr       error
	changed        chan struct{}
}

type Release struct {
	Digest   value.Digest
	Manifest Manifest
	Lock     Lock
}

func Open(config Config) (*Store, error) {
	if config.Directory == "" || config.Admission == nil || config.Trust == nil || config.Retained == nil {
		return nil, errors.New("distribution: directory, admission, trust and retention reader are required")
	}
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	}
	if config.Limits.Files < 1 || config.Limits.ExpandedBytes < 1 || config.Limits.ArchiveBytes < 1 || config.Limits.ManifestBytes < 1 || config.Limits.CompressionRatio < 1 {
		return nil, ErrCapacity
	}
	directory, err := filepath.Abs(config.Directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, privateDirectoryMode); err != nil {
		return nil, err
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	store := &Store{directory: directory, admission: config.Admission, limits: config.Limits, trust: config.Trust, retained: config.Retained, leases: map[value.Digest]int{}, changed: make(chan struct{}), closeDone: make(chan struct{})}
	if err := store.openOwner(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) ImportDirectoryHeld(ctx context.Context, directory string) (lease *Lease, err error) {
	done, err := s.beginOperation(false)
	if err != nil {
		return nil, err
	}
	defer done()
	input, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, input.Close())
		if err != nil && lease != nil {
			err = errors.Join(err, lease.Close())
			lease = nil
		}
	}()
	staged, err := s.stage()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, staged.discard()) }()
	err = fs.WalkDir(input.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if entry.IsDir() {
			return staged.admitPath(name, true)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: only regular files are accepted", ErrInvalid)
		}
		file, err := localfile.OpenRegularIn(input, name)
		if err != nil {
			return err
		}
		opened, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return err
		}
		if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
			_ = file.Close()
			return ErrIntegrity
		}
		copyErr := staged.write(ctx, name, opened.Mode(), file)
		after, statErr := file.Stat()
		if statErr == nil && (!os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime())) {
			statErr = ErrIntegrity
		}
		return errors.Join(copyErr, statErr, file.Close())
	})
	if err != nil {
		return nil, err
	}
	return s.publishHeld(ctx, staged, Source{Location: directory})
}

// Networking and trust policy belong to the application. This operation keeps
// the digest-pinned archive and its installed content under one original lease.
func (s *Store) ImportArchiveHeld(ctx context.Context, fileName string, expected value.Digest) (lease *Lease, err error) {
	done, err := s.beginOperation(false)
	if err != nil {
		return nil, err
	}
	defer done()
	if expected.Validate() != nil {
		return nil, ErrInvalid
	}
	file, err := localfile.OpenRegular(fileName)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil && lease != nil {
			err = errors.Join(err, lease.Close())
			lease = nil
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > s.limits.ArchiveBytes {
		return nil, ErrCapacity
	}
	data, err := readBounded(ctx, file, s.limits.ArchiveBytes)
	if err != nil {
		return nil, err
	}
	if value.Hash(data) != expected {
		return nil, ErrIntegrity
	}
	archive, err := s.limits.openArchive(data)
	if err != nil {
		return nil, err
	}
	staged, err := s.stage()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, staged.discard()) }()
	for _, entry := range archive.File {
		name := strings.TrimSuffix(entry.Name, "/")
		if entry.Mode().Type() != 0 && entry.Mode().Type() != fs.ModeDir {
			return nil, ErrInvalid
		}
		if entry.Mode().IsDir() {
			if err := staged.admitPath(name, true); err != nil {
				return nil, err
			}
			continue
		}
		if !s.limits.admitsExpansion(entry.UncompressedSize64, entry.CompressedSize64) {
			return nil, ErrCapacity
		}
		input, err := entry.Open()
		if err != nil {
			return nil, err
		}
		if err := errors.Join(staged.write(ctx, name, entry.Mode(), input), input.Close()); err != nil {
			return nil, err
		}
	}
	return s.publishHeld(ctx, staged, Source{Location: fileName, ArchiveDigest: expected})
}

func (s *Store) Inspect(ctx context.Context, digest value.Digest) (Release, error) {
	done, err := s.beginOperation(false)
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

type Lease struct {
	store   *Store
	release Release
	mu      sync.Mutex
	closed  bool
}

func (s *Store) Acquire(ctx context.Context, digest value.Digest) (_ *Lease, err error) {
	done, err := s.beginOperation(false)
	if err != nil {
		return nil, err
	}
	defer done()
	if digest.Validate() != nil {
		return nil, ErrInvalid
	}
	// Claim the content before reading it. The gate protects this claim and
	// collection; hashing the immutable tree does not block unrelated admission.
	lease := &Lease{store: s, release: Release{Digest: digest}}
	if err := s.admission.Under(ctx, func() error {
		s.mu.Lock()
		s.leases[digest]++
		s.mu.Unlock()
		return nil
	}); err != nil {
		return nil, err
	}
	release, err := s.inspect(ctx, digest)
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	lease.release = release
	return lease, nil
}

func (l *Lease) Read(ctx context.Context, name string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, frame.ErrClosed
	}
	return l.store.readArtifact(ctx, l.release, name)
}

// Executable returns only a verified executable in the immutable leased tree.
// The lease must remain held through creation and the execution's protection.
func (l *Lease) Executable(ctx context.Context, name string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return "", frame.ErrClosed
	}
	var executable bool
	for _, artifact := range l.release.Manifest.Artifacts {
		if artifact.Path == name {
			executable = artifact.Executable
			break
		}
	}
	if !executable {
		return "", ErrInvalid
	}
	if _, err := l.store.readArtifact(ctx, l.release, name); err != nil {
		return "", err
	}
	return filepath.Join(l.store.directory, l.release.Digest.String(), filepath.FromSlash(name)), nil
}

func (l *Lease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	l.store.leases[l.release.Digest]--
	if l.store.leases[l.release.Digest] == 0 {
		delete(l.store.leases, l.release.Digest)
	}
	l.store.notifyLocked()
	return nil
}

func (s *Store) Remove(ctx context.Context, digest value.Digest) (err error) {
	done, err := s.beginOperation(false)
	if err != nil {
		return err
	}
	defer done()
	if digest.Validate() != nil {
		return ErrInvalid
	}
	retiring, temporary, identity, err := s.privateDirectory(retiringDirectoryPrefix)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, temporary.Close(), s.removePrivate(retiring, identity)) }()
	err = s.admission.Under(ctx, func() (err error) {
		s.mu.Lock()
		busy := s.leases[digest] != 0
		s.mu.Unlock()
		if busy {
			return frame.ErrInUse
		}
		retained, err := s.retained(ctx, digest)
		if err != nil {
			return err
		}
		if retained {
			return frame.ErrInUse
		}
		info, err := s.root.Lstat(digest.String())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return ErrIntegrity
		}
		releaseRoot, err := s.root.OpenRoot(digest.String())
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, releaseRoot.Close()) }()
		opened, err := releaseRoot.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			return errors.Join(ErrIntegrity, err)
		}
		placeholder, err := s.root.Lstat(retiring)
		if err != nil || !os.SameFile(identity, placeholder) {
			return errors.Join(ErrIntegrity, err)
		}
		if err := s.root.Remove(retiring); err != nil {
			return err
		}
		// Older macOS filesystems require the renamed directory to be writable.
		// Admission has excluded leases and retention; its contents stay sealed.
		if err := releaseRoot.Chmod(".", privateDirectoryMode); err != nil {
			return err
		}
		err = s.root.Rename(digest.String(), retiring)
		if err != nil {
			restoreErr := releaseRoot.Chmod(".", info.Mode().Perm())
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			return errors.Join(err, restoreErr)
		}
		identity = info
		return s.lock.Sync()
	})
	// Physical deletion follows withdrawal from the public namespace. A later
	// publication of the same digest cannot be mistaken for this old tree.
	return err
}

func (s *Store) readArtifact(ctx context.Context, release Release, name string) ([]byte, error) {
	done, err := s.beginOperation(true)
	if err != nil {
		return nil, err
	}
	defer done()
	var found *Artifact
	for _, artifact := range release.Manifest.Artifacts {
		if artifact.Path == name {
			copy := artifact
			found = &copy
			break
		}
	}
	if found == nil {
		return nil, ErrInvalid
	}
	root, err := s.openRelease(release.Digest)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := readRegular(ctx, root, name, found.Size)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != found.Size || value.Hash(data) != found.Digest {
		return nil, ErrIntegrity
	}
	return data, nil
}

func (s *Store) publishHeld(ctx context.Context, staged *staging, source Source) (*Lease, error) {
	release, err := s.validate(ctx, staged.root)
	if err != nil {
		return nil, err
	}
	if err := s.checkTrust(ctx, source, release); err != nil {
		return nil, errors.Join(ErrUntrusted, err)
	}
	if err := sealContents(staged.root); err != nil {
		return nil, err
	}
	lease := &Lease{store: s, release: release}
	var existing fs.FileInfo
	err = s.admission.Under(ctx, func() error {
		destination := release.Digest.String()
		if info, err := s.root.Lstat(destination); err == nil {
			existing = info
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			info, err := s.root.Lstat(staged.name)
			if err != nil || !os.SameFile(staged.identity, info) {
				return errors.Join(ErrIntegrity, err)
			}
			if err := s.root.Rename(staged.name, destination); err != nil {
				return err
			}
			// Seal the renamed root before admission can grant its first lease.
			// A failed seal leaves a directory that openRelease refuses.
			if err := staged.root.Chmod(".", sealedDirectoryMode); err != nil {
				return err
			}
			if err := s.lock.Sync(); err != nil {
				return err
			}
		}
		// Publication and its original content claim are one admission. No
		// collector can remove a candidate between publication and installation.
		s.mu.Lock()
		s.leases[release.Digest]++
		s.mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if existing != nil {
		cached, err := s.admitCached(ctx, source, release.Digest, existing)
		if err != nil {
			return nil, errors.Join(err, lease.Close())
		}
		lease.release = cached
	}
	return lease, nil
}

// A crash can leave a renamed root writable. The original lease pins that
// directory while application trust evaluates the exact cached projection.
func (s *Store) admitCached(ctx context.Context, source Source, digest value.Digest, original fs.FileInfo) (_ Release, err error) {
	if !original.IsDir() || original.Mode().Perm() != sealedDirectoryMode && original.Mode().Perm() != privateDirectoryMode {
		return Release{}, ErrIntegrity
	}
	root, err := s.root.OpenRoot(digest.String())
	if err != nil {
		return Release{}, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, original) {
		return Release{}, errors.Join(ErrIntegrity, err)
	}
	cached, err := s.validate(ctx, root)
	if err != nil || cached.Digest != digest {
		return Release{}, errors.Join(ErrIntegrity, err)
	}
	if err := s.checkTrust(ctx, source, cached); err != nil {
		return Release{}, errors.Join(ErrUntrusted, err)
	}
	if original.Mode().Perm() == sealedDirectoryMode {
		return cached, nil
	}
	err = s.admission.Under(ctx, func() error {
		current, err := s.root.Lstat(digest.String())
		if err != nil || !os.SameFile(current, original) {
			return errors.Join(ErrIntegrity, err)
		}
		// Optional manifest fields can differ without changing a release digest.
		// Seal only the projection that this source's trust callback approved.
		checked, err := s.validate(ctx, root)
		if err != nil || !reflect.DeepEqual(checked, cached) {
			return errors.Join(ErrIntegrity, err)
		}
		if err := sealContents(root); err != nil {
			return err
		}
		if err := root.Chmod(".", sealedDirectoryMode); err != nil {
			return err
		}
		return s.lock.Sync()
	})
	return cached, err
}

func (s *Store) validate(ctx context.Context, root *os.Root) (Release, error) {
	manifestBytes, err := readRegular(ctx, root, ManifestName, s.limits.ManifestBytes)
	if err != nil {
		return Release{}, err
	}
	manifest, err := ParseManifest(manifestBytes)
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
	lockBytes, err := readRegular(ctx, root, LockName, s.limits.ManifestBytes)
	if err != nil {
		return Release{}, err
	}
	lock, err := ParseLock(lockBytes)
	if err != nil {
		return Release{}, err
	}
	if err := lock.Verify(manifest); err != nil {
		return Release{}, err
	}
	expected := map[string]Artifact{}
	for _, artifact := range manifest.Artifacts {
		expected[artifact.Path] = artifact
	}
	var files int
	var size int64
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		files++
		if files > s.limits.Files {
			return ErrCapacity
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrInvalid
		}
		size += info.Size()
		if size > s.limits.ExpandedBytes {
			return ErrCapacity
		}
		if isReleaseMetadata(name) {
			return nil
		}
		artifact, ok := expected[name]
		if !ok || artifact.Size != info.Size() || (info.Mode().Perm()&fileExecuteMask != 0) != artifact.Executable {
			return ErrIntegrity
		}
		data, err := readRegular(ctx, root, name, artifact.Size)
		if err != nil {
			return err
		}
		if value.Hash(data) != artifact.Digest {
			return ErrIntegrity
		}
		delete(expected, name)
		return nil
	})
	if err != nil {
		return Release{}, err
	}
	if len(expected) != 0 {
		return Release{}, ErrIntegrity
	}
	return Release{Digest: digest, Manifest: manifest, Lock: lock}, nil
}

type staging struct {
	store    *Store
	name     string
	identity fs.FileInfo
	root     *os.Root
	limits   Limits
	paths    archivePaths
	bytes    int64
}

func (s *Store) stage() (*staging, error) {
	name, root, identity, err := s.privateDirectory(stagingDirectoryPrefix)
	if err != nil {
		return nil, err
	}
	return &staging{store: s, name: name, identity: identity, root: root, limits: s.limits, paths: archivePaths{}}, nil
}

func (s *staging) discard() error {
	return errors.Join(s.root.Close(), s.store.removePrivate(s.name, s.identity))
}

func (s *staging) admitPath(name string, directory bool) error {
	if err := s.paths.admit(name, directory); err != nil {
		return err
	}
	if len(s.paths) > s.limits.Files {
		return ErrCapacity
	}
	return nil
}

func (s *staging) write(ctx context.Context, name string, mode fs.FileMode, reader io.Reader) error {
	if mode.Type() != 0 || mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return ErrInvalid
	}
	if err := s.admitPath(name, false); err != nil {
		return err
	}
	if err := s.root.MkdirAll(path.Dir(name), privateDirectoryMode); err != nil {
		return err
	}
	limit := s.limits.ExpandedBytes - s.bytes
	if isReleaseMetadata(name) {
		limit = min(limit, s.limits.ManifestBytes)
	}
	data, err := readBounded(ctx, reader, limit)
	if err != nil {
		return err
	}
	s.bytes += int64(len(data))
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode|mode.Perm()&fileExecuteMask)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

func sealContents(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		mode := sealedFileMode
		if entry.IsDir() {
			mode = sealedDirectoryMode
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode |= info.Mode().Perm() & fileExecuteMask
		}
		return root.Chmod(name, mode)
	})
}

func removeSealed(directory string) error {
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	err := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(name, privateDirectoryMode)
		}
		return nil
	})
	return errors.Join(err, os.RemoveAll(directory))
}

func readRegular(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, error) {
	if !validPath(name) {
		return nil, ErrInvalid
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrIntegrity
	}
	file, err := localfile.OpenRegularIn(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, ErrIntegrity
	}
	data, err := readBounded(ctx, file, limit)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, ErrIntegrity
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
func readBounded(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, ErrCapacity
	}
	reader = contextReader{ctx, reader}
	data, err := io.ReadAll(io.LimitReader(reader, limit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) < limit {
		return data, nil
	}
	// Probe separately so the full positive int64 range cannot overflow into
	// a negative LimitedReader bound and silently discard the original bytes.
	var extra [1]byte
	if n, err := io.ReadFull(reader, extra[:]); n != 0 {
		return nil, ErrCapacity
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data, nil
}

func EncodeManifest(manifest Manifest) ([]byte, error) {
	digest, err := manifest.Identity()
	if err != nil {
		return nil, err
	}
	manifest.Release = digest
	return json.MarshalIndent(manifest, "", "  ")
}

// Policy receives only detached metadata; it cannot rewrite verified content.
func (s *Store) checkTrust(ctx context.Context, source Source, release Release) error {
	view := release.snapshot()
	return s.trust(ctx, source, view.Manifest, view.Lock)
}
