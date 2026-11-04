package sqlite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/localfile"
	framevalue "github.com/Tangerg/frame/value"
)

type pragmaRequirement struct {
	query string
	want  int
}

// Open returns a sealed Store together with an error only when its original
// cleanup is unconfirmed. That Store permits Close and identity diagnostics,
// not database work.
func Open(ctx context.Context, config Config) (result *Store, err error) {
	if config.Admission == nil || config.Path == "" || config.Path == ":memory:" {
		return nil, errors.New("sqlite: a file path and the host admission are required")
	}
	path, err := databasePath(config.Path)
	if err != nil {
		return nil, err
	}
	lock, err := acquireAuthority(path)
	if err != nil {
		return nil, err
	}
	store := &Store{lock: lock, admission: config.Admission, owners: make(map[string]*Owner), maxRequestBytes: config.MaxRequestBytes, maxSubmissions: config.MaxSubmissionsPerOwner, maxRealmHistoryBytes: config.MaxRealmHistoryBytes, projectInstallation: config.ProjectInstallation}
	store.control = &Owner{store: store, id: installationOwnerID}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.Close())
			select {
			case <-store.closeDone:
			default:
				result = store
			}
		}
	}()
	if err := store.resolveLimits(); err != nil {
		return nil, err
	}
	owners := slices.SortedFunc(slices.Values(config.Owners), func(a, b OwnerSchema) int { return strings.Compare(a.ID, b.ID) })
	statements, digest, err := store.registerOwners(owners)
	if err != nil {
		return nil, err
	}
	expectedShape, err := schemaShape(ctx, statements)
	if err != nil {
		return nil, fmt.Errorf("sqlite: owner schema: %w", err)
	}
	if err := store.openDatabase(ctx, path); err != nil {
		return nil, err
	}
	if err := store.openEpoch(ctx, owners, digest, expectedShape); err != nil {
		return nil, err
	}
	var unresolved int
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM frame_submissions WHERE state=? OR synchronized=0)+(SELECT count(*) FROM frame_attempts WHERE state IN (?,?))+(SELECT count(*) FROM frame_use_approvals WHERE state=?)`, submissionPrepared, AttemptApproved, AttemptHandoff, useApproved).Scan(&unresolved); err != nil {
		return nil, err
	}
	if unresolved != 0 {
		store.admission.RequireRecovery(ErrIndeterminate)
	}
	return store, nil
}

func (s *Store) resolveLimits() error {
	if s.maxRequestBytes == 0 {
		s.maxRequestBytes = defaultMaxRequestBytes
	}
	if s.maxSubmissions == 0 {
		s.maxSubmissions = defaultMaxSubmissions
	}
	if s.maxRealmHistoryBytes == 0 {
		s.maxRealmHistoryBytes = defaultMaxRealmHistoryBytes
	}
	if s.maxRequestBytes < 1 || s.maxSubmissions < 1 || s.maxRealmHistoryBytes < 1 {
		return errors.New("sqlite: request, submission and realm history limits must be positive")
	}
	return nil
}

// registerOwners admits the complete owner set. Its declarations, in owner
// order, are the schema identity a reopened database must match.
func (s *Store) registerOwners(owners []OwnerSchema) ([]string, framevalue.Digest, error) {
	statements := []string{schema}
	declarations := schema
	for _, owner := range owners {
		if !frame.ValidName(owner.ID) || strings.HasPrefix(owner.ID, reservedOwnerPrefix) || owner.References == nil {
			return nil, framevalue.Digest{}, errors.New("sqlite: owner requires an identity and a reference reader")
		}
		if _, duplicate := s.owners[owner.ID]; duplicate {
			return nil, framevalue.Digest{}, errors.New("sqlite: duplicate owner")
		}
		s.owners[owner.ID] = &Owner{store: s, id: owner.ID, references: owner.References}
		statements = append(statements, owner.Statements...)
		declarations += "\n-- owner " + owner.ID + "\n" + strings.Join(owner.Statements, ";\n")
	}
	return statements, framevalue.Hash([]byte(declarations)), nil
}

func databasePath(requested string) (string, error) {
	path, err := filepath.Abs(requested)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), localfile.PrivateDirectoryMode); err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	path = filepath.Join(parent, filepath.Base(path))
	if info, statErr := os.Lstat(path); statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return "", errors.New("sqlite: database must be a regular file")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	return path, nil
}

func (s *Store) openDatabase(ctx context.Context, path string) error {
	database, err := openDatabase(storeDataSource(path))
	if err != nil {
		return err
	}
	s.db = database
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	var journal string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&journal); err != nil {
		return err
	}
	if journal != walJournalMode {
		return errors.New("sqlite: wal was not enabled")
	}
	for _, requirement := range []pragmaRequirement{
		{query: "PRAGMA synchronous", want: synchronousFull},
		{query: "PRAGMA read_uncommitted", want: pragmaDisabled},
		{query: "PRAGMA foreign_keys", want: pragmaEnabled},
	} {
		var got int
		if err := s.db.QueryRowContext(ctx, requirement.query).Scan(&got); err != nil {
			return err
		}
		if got != requirement.want {
			return fmt.Errorf("sqlite: %s was not applied", requirement.query)
		}
	}
	return nil
}

func storeDataSource(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + fmt.Sprintf(
		"?_pragma=foreign_keys(%d)&_pragma=synchronous(%d)&_pragma=read_uncommitted(%d)&_pragma=busy_timeout(%d)",
		pragmaEnabled, synchronousFull, pragmaDisabled, busyTimeout/time.Millisecond,
	)
}

func (s *Store) openEpoch(ctx context.Context, owners []OwnerSchema, digest, expectedShape framevalue.Digest) error {
	return s.transaction(ctx, func(tx Executor) error {
		actualShape, err := readSchemaShape(ctx, tx)
		if err != nil {
			return err
		}
		if actualShape.IsZero() {
			return s.createSchema(ctx, tx, owners, digest)
		}
		if actualShape != expectedShape {
			return errors.New("sqlite: incompatible database schema; open a fresh data directory")
		}
		return s.resumeEpoch(ctx, tx, digest)
	})
}

func (s *Store) createSchema(ctx context.Context, tx Executor, owners []OwnerSchema, digest framevalue.Digest) error {
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	s.id = hex.EncodeToString(random[:])
	s.epoch = 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO frame_meta VALUES(1,?,?,1)`, s.id, digest); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO frame_owners(id) VALUES(?)`, s.control.id); err != nil {
		return err
	}
	for _, owner := range owners {
		for _, statement := range owner.Statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO frame_owners(id) VALUES(?)`, owner.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) resumeEpoch(ctx context.Context, tx Executor, digest framevalue.Digest) error {
	var storedDigest framevalue.Digest
	if err := tx.QueryRowContext(ctx, `SELECT store_id,schema_digest,epoch FROM frame_meta WHERE singleton=1`).Scan(&s.id, &storedDigest, &s.epoch); err != nil {
		return fmt.Errorf("sqlite: incompatible schema: %w", err)
	}
	if storedDigest != digest {
		return errors.New("sqlite: incompatible owner schema; open a fresh data directory")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM frame_realm_receipts`); err != nil {
		return err
	}
	s.epoch++
	_, err := tx.ExecContext(ctx, `UPDATE frame_meta SET epoch=? WHERE singleton=1`, s.epoch)
	return err
}
