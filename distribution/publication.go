package distribution

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"reflect"

	"github.com/Tangerg/frame/value"
)

func (s *storeAuthority) publishHeld(ctx context.Context, staged *staging, source Source) (result *Lease, err error) {
	release, err := s.validate(ctx, staged.root, unsealedTree)
	if err != nil {
		return nil, err
	}
	if err := s.checkTrust(ctx, source, release); err != nil {
		return nil, errors.Join(ErrUntrusted, err)
	}
	if err := sealReleaseContents(ctx, staged.root, release.Manifest, s.limits.Entries); err != nil {
		return nil, err
	}
	var lease *Lease
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
			// Some filesystems require a writable source directory for rename.
			// Seal and sync its original inode before publishing the parent entry.
			if err := sealEntry(staged.root, ".", staged.identity, sealedDirectoryMode); err != nil {
				return err
			}
			if err := s.lock.Sync(); err != nil {
				return err
			}
		}
		// Publication and its original content claim are one admission. No
		// collector can remove a candidate between publication and installation.
		lease = s.claimRelease(release)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Until this call transfers the lease, cached trust may refuse or panic.
	defer func() {
		if result == nil {
			err = errors.Join(err, lease.Close())
		}
	}()
	if existing != nil {
		cached, err := s.admitCached(ctx, source, release.Digest, existing)
		if err != nil {
			return nil, err
		}
		lease.claim.release = cached
	}
	return lease, nil
}

// A crash can leave a renamed root writable. The original lease pins that
// directory while application trust evaluates the exact cached projection.
func (s *storeAuthority) admitCached(ctx context.Context, source Source, digest value.Digest, original fs.FileInfo) (_ Release, err error) {
	if !original.IsDir() || original.Mode()&specialFileModes != 0 || original.Mode().Perm() != sealedDirectoryMode && original.Mode().Perm() != privateDirectoryMode {
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
	cached, err := s.validate(ctx, root, unsealedTree)
	if err != nil || cached.Digest != digest {
		return Release{}, errors.Join(ErrIntegrity, err)
	}
	if err := s.checkTrust(ctx, source, cached); err != nil {
		return Release{}, errors.Join(ErrUntrusted, err)
	}
	err = s.admission.Under(ctx, func() error {
		current, err := s.root.Lstat(digest.String())
		if err != nil || !os.SameFile(current, original) {
			return errors.Join(ErrIntegrity, err)
		}
		// Optional manifest fields can differ without changing a release digest.
		// Seal only the projection that this source's trust callback approved.
		checked, err := s.validate(ctx, root, unsealedTree)
		if err != nil || !reflect.DeepEqual(checked, cached) {
			return errors.Join(ErrIntegrity, err)
		}
		if err := sealReleaseContents(ctx, root, cached.Manifest, s.limits.Entries); err != nil {
			return err
		}
		if err := sealEntry(root, ".", original, sealedDirectoryMode); err != nil {
			return err
		}
		if _, err := s.validate(ctx, root, sealedTree); err != nil {
			return err
		}
		return s.lock.Sync()
	})
	return cached, err
}

// Policy receives only detached metadata; it cannot rewrite verified content.
func (s *storeAuthority) checkTrust(ctx context.Context, source Source, release Release) error {
	view := release.snapshot()
	return s.trust(ctx, source, view.Manifest, view.Lock)
}
