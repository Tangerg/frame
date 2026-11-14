package distribution

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	framehost "github.com/Tangerg/frame/host"
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

// resolve selects the default of each zero bound independently, so narrowing
// one limit keeps the others at their defaults.
func (l Limits) resolve() (Limits, error) {
	defaults := DefaultLimits()
	l.Entries = cmp.Or(l.Entries, defaults.Entries)
	l.ExpandedBytes = cmp.Or(l.ExpandedBytes, defaults.ExpandedBytes)
	l.ArchiveBytes = cmp.Or(l.ArchiveBytes, defaults.ArchiveBytes)
	l.ManifestBytes = cmp.Or(l.ManifestBytes, defaults.ManifestBytes)
	l.CompressionRatio = cmp.Or(l.CompressionRatio, defaults.CompressionRatio)
	if l.Entries < 1 || l.ExpandedBytes < 1 || l.ArchiveBytes < 1 || l.ManifestBytes < 1 {
		return Limits{}, ErrCapacity
	}
	return l, nil
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
	Admission framehost.Admission
	Limits    Limits
	Trust     Trust
	Retained  func(context.Context, value.Digest) (bool, error)
}

// Store is a value handle; copies and issued leases retain one directory owner.
type Store struct{ *storeAuthority }

type storeAuthority struct {
	directory  string
	root       *os.Root
	identity   fs.FileInfo
	lock       *os.File
	admission  framehost.Admission
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

func Open(config Config) (Store, error) {
	if config.Directory == "" || !config.Admission.Valid() || config.Trust == nil || config.Retained == nil {
		return Store{}, errors.New("distribution: directory, admission, trust and retention reader are required")
	}
	limits, err := config.Limits.resolve()
	if err != nil {
		return Store{}, err
	}
	config.Limits = limits
	directory, err := filepath.Abs(config.Directory)
	if err != nil {
		return Store{}, err
	}
	if err := os.MkdirAll(directory, privateDirectoryMode); err != nil {
		return Store{}, err
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return Store{}, err
	}
	store := &storeAuthority{directory: directory, admission: config.Admission, limits: config.Limits, trust: config.Trust, retained: config.Retained, leases: map[value.Digest]int{}, changed: make(chan struct{}), closeDone: make(chan struct{})}
	if err := store.openOwner(); err != nil {
		return Store{}, err
	}
	return Store{store}, nil
}

// Path projects an immutable release location for diagnostics and declarations.
// It performs no admission: actual reads and execution require an acquired Lease.
func (s *storeAuthority) Path(digest value.Digest) (string, error) {
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
