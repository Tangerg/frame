package distribution

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type Limits struct {
	Entries          int
	ExpandedBytes    int64
	ArchiveBytes     int64
	ManifestBytes    int64
	CompressionRatio uint64
}

const (
	defaultMaxEntries       = 10000
	defaultExpandedBytes    = 512 << 20
	defaultArchiveBytes     = 256 << 20
	defaultManifestBytes    = 4 << 20
	defaultCompressionRatio = 200
)

func DefaultLimits() Limits {
	return Limits{Entries: defaultMaxEntries, ExpandedBytes: defaultExpandedBytes, ArchiveBytes: defaultArchiveBytes, ManifestBytes: defaultManifestBytes, CompressionRatio: defaultCompressionRatio}
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
	directory  string
	root       *os.Root
	identity   fs.FileInfo
	lock       *os.File
	admission  *frame.Admission
	limits     Limits
	trust      Trust
	retained   func(context.Context, value.Digest) (bool, error)
	mu         sync.Mutex
	leases     map[value.Digest]int
	operations int
	phase      storePhase
	closeDone  chan struct{}
	closeErr   error
	changed    chan struct{}
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
	if config.Limits.Entries < 1 || config.Limits.ExpandedBytes < 1 || config.Limits.ArchiveBytes < 1 || config.Limits.ManifestBytes < 1 || config.Limits.CompressionRatio < 1 {
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

// Path projects an immutable release location for diagnostics and declarations.
// It performs no admission: actual reads and execution require an acquired Lease.
func (s *Store) Path(digest value.Digest) (string, error) {
	done, err := s.beginOperation(leaseOperation)
	if err != nil {
		return "", err
	}
	defer done()
	if digest.Validate() != nil {
		return "", ErrInvalid
	}
	return filepath.Join(s.directory, digest.String()), nil
}
