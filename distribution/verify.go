package distribution

import (
	"context"
	"errors"
	"os"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
)

// VerifyArchive validates a publisher artifact in disposable storage. It makes
// no publisher trust decision and never installs code into an application Host.
func VerifyArchive(ctx context.Context, archive string, digest integrity.Digest, limits Limits) (_ Release, err error) {
	directory, err := os.MkdirTemp("", "frame-verify-")
	if err != nil {
		return Release{}, err
	}
	defer func() { err = errors.Join(err, removeSealed(directory)) }()
	store, err := Open(Config{
		Directory: directory, Admission: frame.NewAdmission(), Limits: limits,
		Trust:    func(context.Context, Source, Manifest, Lock) error { return nil },
		Retained: func(context.Context, integrity.Digest) (bool, error) { return true, nil },
	})
	if err != nil {
		return Release{}, err
	}
	defer func() { err = errors.Join(err, store.Close(context.Background())) }()
	return store.ImportArchive(ctx, archive, digest)
}
