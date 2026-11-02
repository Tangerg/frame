// Package sqlite owns the single-authority durable submission and execution
// boundaries. Applications retain ownership of their business tables.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
	_ "modernc.org/sqlite"
)

var (
	ErrConflict          = errors.New("sqlite: submission identity conflict")
	ErrEvidenceExpired   = errors.New("sqlite: settlement evidence expired")
	errCommitUnresolved  = errors.New("sqlite: original commit requires reconciliation")
	ErrIndeterminate     = errors.New("sqlite: submission settlement is indeterminate")
	ErrUnsettled         = errors.New("sqlite: submission is not settled and synchronized")
	ErrUnknownSubmission = errors.New("sqlite: submission was not issued by this owner")
)

const (
	defaultMaxRequestBytes = 1 << 20
	defaultMaxSubmissions  = 4096
	maxIdentifierBytes     = 256
	synchronousFull        = 2
	pragmaEnabled          = 1
	pragmaDisabled         = 0
)

type pragmaRequirement struct {
	query string
	want  int
}

type Reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Executor interface {
	Reader
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// OwnerSchema is a trusted application integration, never a plugin callback.
// References reads the owner's authoritative business facts in the supplied
// current transaction; Frame does not mirror those facts in a reference ledger.
type OwnerSchema struct {
	ID         string
	Statements []string
	References func(context.Context, Reader, frame.InstallationID) ([]frame.Dependency, error)
}

type Config struct {
	Path                   string
	Admission              *frame.Admission
	Owners                 []OwnerSchema
	MaxRequestBytes        int
	MaxSubmissionsPerOwner int
	// ProjectInstallation encodes application-owned indexes or foreign-key
	// projections in the same commit. It cannot originate an installation change.
	ProjectInstallation func(context.Context, Executor, frame.InstallationChange) error
}

type Store struct {
	db                    *sql.DB
	expiredTransactionRow *sql.Row
	lock                  *os.File
	admission             *frame.Admission
	id                    string
	epoch                 uint64
	owners                map[string]*Owner
	control               *Owner
	maxRequestBytes       int
	maxSubmissions        int
	lifecycleMu           sync.Mutex
	transactions          sync.WaitGroup
	closing               bool
	databaseSealed        bool
	databaseCloseErr      error
	unconfirmed           []*sql.Conn
	closeOnce             sync.Once
	closeRequests         chan chan error
	closeDone             chan struct{}
	closeErr              error
	projectInstallation   func(context.Context, Executor, frame.InstallationChange) error
}

type Owner struct {
	store      *Store
	id         string
	references func(context.Context, Reader, frame.InstallationID) ([]frame.Dependency, error)
}

const schema = `
CREATE TABLE frame_meta (singleton INTEGER PRIMARY KEY CHECK(singleton=1),store_id TEXT NOT NULL,schema_digest TEXT NOT NULL,epoch INTEGER NOT NULL CHECK(epoch>0));
CREATE TABLE frame_owners (id TEXT PRIMARY KEY,next_sequence INTEGER NOT NULL DEFAULT 0,compacted_through INTEGER NOT NULL DEFAULT 0 CHECK(compacted_through<=next_sequence));
CREATE TABLE frame_submissions (owner_id TEXT NOT NULL REFERENCES frame_owners(id),sequence INTEGER NOT NULL,digest TEXT NOT NULL,request BLOB NOT NULL,state TEXT NOT NULL CHECK(state IN ('` + string(submissionPrepared) + `','` + string(submissionCommitted) + `','` + string(submissionNotCommitted) + `')),evidence BLOB,synchronized INTEGER NOT NULL DEFAULT 0 CHECK(synchronized IN (0,1)),PRIMARY KEY(owner_id,sequence));
CREATE TABLE frame_evidence_references (owner_id TEXT NOT NULL,sequence INTEGER NOT NULL,holder TEXT NOT NULL,PRIMARY KEY(owner_id,sequence,holder),FOREIGN KEY(owner_id,sequence) REFERENCES frame_submissions(owner_id,sequence));
CREATE TABLE frame_attempts (id TEXT PRIMARY KEY,epoch INTEGER NOT NULL,request BLOB NOT NULL,request_digest TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('` + string(attemptApproved) + `','` + string(attemptHandoff) + `','` + string(attemptReleased) + `')),proof BLOB);
CREATE INDEX frame_attempts_by_state ON frame_attempts(state,id);
CREATE TABLE frame_installations (id TEXT PRIMARY KEY,record BLOB NOT NULL);
CREATE TABLE frame_use_approvals (id TEXT PRIMARY KEY,request BLOB NOT NULL,execution_id TEXT GENERATED ALWAYS AS (json_extract(CAST(request AS TEXT),'$.ExecutionID')) VIRTUAL,request_digest TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('` + string(useApproved) + `','` + string(useReleased) + `')),proof BLOB);
CREATE INDEX frame_use_approvals_by_state ON frame_use_approvals(state,id);
CREATE INDEX frame_uses_by_execution ON frame_use_approvals(execution_id,id);
CREATE TABLE frame_realms (id TEXT NOT NULL,epoch TEXT NOT NULL,operation_id TEXT NOT NULL UNIQUE,resume_verifier TEXT NOT NULL,PRIMARY KEY(id,epoch));
CREATE TABLE frame_exit_proofs (attempt_id TEXT NOT NULL REFERENCES frame_attempts(id),scope TEXT NOT NULL,proof_id TEXT NOT NULL,evidence BLOB NOT NULL,PRIMARY KEY(attempt_id,proof_id),UNIQUE(attempt_id,scope));
` + realmReceiptSchema

// Open returns a sealed Store together with an error only when its original
// cleanup is unconfirmed. That Store permits Close and identity diagnostics,
// not database work.
func Open(ctx context.Context, config Config) (result *Store, err error) {
	if config.Admission == nil || config.Path == "" || config.Path == ":memory:" {
		return nil, errors.New("sqlite: a file path and the host admission are required")
	}
	path, err := filepath.Abs(config.Path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	if info, statErr := os.Lstat(path); statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("sqlite: database must be a regular file")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	lock, err := acquireAuthority(path)
	if err != nil {
		return nil, err
	}
	store := &Store{lock: lock, admission: config.Admission, owners: make(map[string]*Owner), maxRequestBytes: config.MaxRequestBytes, maxSubmissions: config.MaxSubmissionsPerOwner, projectInstallation: config.ProjectInstallation}
	store.control = &Owner{store: store, id: "frame.installations"}
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
	if store.maxRequestBytes == 0 {
		store.maxRequestBytes = defaultMaxRequestBytes
	}
	if store.maxRequestBytes < 1 {
		return nil, errors.New("sqlite: request byte limit must be positive")
	}
	if store.maxSubmissions == 0 {
		store.maxSubmissions = defaultMaxSubmissions
	}
	if store.maxSubmissions < 1 {
		return nil, errors.New("sqlite: submission capacity must be positive")
	}
	owners := slices.Clone(config.Owners)
	slices.SortFunc(owners, func(a, b OwnerSchema) int { return strings.Compare(a.ID, b.ID) })
	statements := []string{schema}
	declarations := schema
	for _, owner := range owners {
		if !validID(owner.ID) || strings.HasPrefix(owner.ID, "frame.") || owner.References == nil {
			return nil, errors.New("sqlite: owner requires an identity and a reference reader")
		}
		if _, duplicate := store.owners[owner.ID]; duplicate {
			return nil, errors.New("sqlite: duplicate owner")
		}
		store.owners[owner.ID] = &Owner{store: store, id: owner.ID, references: owner.References}
		statements = append(statements, owner.Statements...)
		declarations += "\n-- owner " + owner.ID + "\n" + strings.Join(owner.Statements, ";\n")
	}
	digest := framevalue.Hash([]byte(declarations))
	expectedShape, err := schemaShape(ctx, statements)
	if err != nil {
		return nil, fmt.Errorf("sqlite: owner schema: %w", err)
	}
	store.db, err = sql.Open("sqlite", storeDataSource(path))
	if err != nil {
		return nil, err
	}
	// database/sql exposes a single-row error via Scan only. A canceled pool
	// query produces a detached immutable error row without opening a connection.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	store.expiredTransactionRow = store.db.QueryRowContext(expired, "")
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)
	var journal string
	if err := store.db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&journal); err != nil {
		return nil, err
	}
	if journal != "wal" {
		return nil, errors.New("sqlite: wal was not enabled")
	}
	for _, requirement := range []pragmaRequirement{
		{query: "PRAGMA synchronous", want: synchronousFull},
		{query: "PRAGMA read_uncommitted", want: pragmaDisabled},
		{query: "PRAGMA foreign_keys", want: pragmaEnabled},
	} {
		var got int
		if err := store.db.QueryRowContext(ctx, requirement.query).Scan(&got); err != nil {
			return nil, err
		}
		if got != requirement.want {
			return nil, fmt.Errorf("sqlite: %s was not applied", requirement.query)
		}
	}
	err = store.transaction(ctx, func(tx Executor) error {
		actualShape, err := readSchemaShape(ctx, tx)
		if err != nil {
			return err
		}
		if actualShape.IsZero() {
			if _, err := tx.ExecContext(ctx, schema); err != nil {
				return err
			}
			var random [24]byte
			if _, err := rand.Read(random[:]); err != nil {
				return err
			}
			store.id = hex.EncodeToString(random[:])
			store.epoch = 1
			if _, err := tx.ExecContext(ctx, `INSERT INTO frame_meta VALUES(1,?,?,1)`, store.id, digest); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO frame_owners(id) VALUES(?)`, store.control.id); err != nil {
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
		if actualShape != expectedShape {
			return errors.New("sqlite: incompatible database schema; open a fresh data directory")
		}
		var storedDigest framevalue.Digest
		if err := tx.QueryRowContext(ctx, `SELECT store_id,schema_digest,epoch FROM frame_meta WHERE singleton=1`).Scan(&store.id, &storedDigest, &store.epoch); err != nil {
			return fmt.Errorf("sqlite: incompatible schema: %w", err)
		}
		if storedDigest != digest {
			return errors.New("sqlite: incompatible owner schema; open a fresh data directory")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM frame_realm_receipts`); err != nil {
			return err
		}
		store.epoch++
		_, err = tx.ExecContext(ctx, `UPDATE frame_meta SET epoch=? WHERE singleton=1`, store.epoch)
		return err
	})
	if err != nil {
		return nil, err
	}
	var unresolved int
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM frame_submissions WHERE state=? OR synchronized=0)+(SELECT count(*) FROM frame_attempts WHERE state IN (?,?))+(SELECT count(*) FROM frame_use_approvals WHERE state=?)`, submissionPrepared, attemptApproved, attemptHandoff, useApproved).Scan(&unresolved); err != nil {
		return nil, err
	}
	if unresolved != 0 {
		store.admission.RequireRecovery(ErrIndeterminate)
	}
	return store, nil
}

func schemaShape(ctx context.Context, statements []string) (framevalue.Digest, error) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return framevalue.Digest{}, err
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			return framevalue.Digest{}, err
		}
	}
	return readSchemaShape(ctx, database)
}

func storeDataSource(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + fmt.Sprintf(
		"?_pragma=foreign_keys(%d)&_pragma=synchronous(%d)&_pragma=read_uncommitted(%d)&_pragma=busy_timeout(%d)",
		pragmaEnabled, synchronousFull, pragmaDisabled, busyTimeout/time.Millisecond,
	)
}

func readSchemaShape(ctx context.Context, executor Reader) (framevalue.Digest, error) {
	rows, err := executor.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT GLOB 'sqlite_*' ORDER BY type,name`)
	if err != nil {
		return framevalue.Digest{}, err
	}
	defer rows.Close()
	var objects [][4]string
	for rows.Next() {
		var object [4]string
		if err := rows.Scan(&object[0], &object[1], &object[2], &object[3]); err != nil {
			return framevalue.Digest{}, err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return framevalue.Digest{}, err
	}
	if len(objects) == 0 {
		return framevalue.Digest{}, nil
	}
	data, err := json.Marshal(objects)
	if err != nil {
		return framevalue.Digest{}, err
	}
	return framevalue.Hash(data), nil
}

func (s *Store) ID() string    { return s.id }
func (s *Store) Epoch() uint64 { return s.epoch }
func (s *Store) Owner(id string) (*Owner, error) {
	owner, ok := s.owners[id]
	if !ok {
		return nil, fmt.Errorf("sqlite: required owner %q is unavailable", id)
	}
	return owner, nil
}

func (o *Owner) ID() string { return o.id }

// Held starts a new read after admission. It must not be called from an owner
// mutation callback; that callback already has the current transaction.
func (o *Owner) Held(ctx context.Context, installation frame.InstallationID) ([]frame.Dependency, error) {
	var dependencies []frame.Dependency
	err := o.store.Read(ctx, func(tx Reader) error {
		var err error
		dependencies, err = o.references(ctx, tx, installation)
		return err
	})
	return dependencies, err
}

func validID(value string) bool {
	if value == "" || len(value) > maxIdentifierBytes {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return true
}

// The connection stays exclusively owned until COMMIT or rollback has settled.
// Cancellation never abandons a transaction that can commit after admission opens.
type transactionMode uint8

const (
	writeTransaction transactionMode = iota
	readTransaction
)

func (s *Store) transaction(ctx context.Context, apply func(Executor) error) error {
	return s.transact(ctx, writeTransaction, apply)
}
func (s *Store) transact(ctx context.Context, mode transactionMode, apply func(Executor) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.lifecycleMu.Lock()
	if s.closing {
		s.lifecycleMu.Unlock()
		return errors.Join(frame.ErrClosed, s.admission.RecoveryError())
	}
	s.transactions.Add(1)
	s.lifecycleMu.Unlock()
	defer s.transactions.Done()
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	returnConnection := true
	defer func() {
		if !returnConnection {
			return
		}
		if closeErr := connection.Close(); !errors.Is(closeErr, sql.ErrConnDone) {
			err = errors.Join(err, closeErr)
		}
	}()
	settled := false
	commitIssued := false
	scope := newConnectionScope(connection)
	defer func() {
		recovered := recover()
		scope.seal()
		if settled {
			if recovered != nil {
				panic(recovered)
			}
			return
		}
		_, rollbackErr := connection.ExecContext(context.Background(), `ROLLBACK`)
		if rollbackErr != nil || scope.failure() != nil {
			if rollbackErr != nil && commitIssued {
				err = errors.Join(err, errCommitUnresolved)
			}
			released, closeErr := closeOriginalConnection(connection)
			if !released {
				returnConnection = false
				s.retainUnconfirmedConnection(connection, errors.Join(rollbackErr, closeErr))
				err = errors.Join(err, ErrIndeterminate, closeErr)
			}
			err = errors.Join(err, rollbackErr)
		}
		err = scope.include(err)
		if recovered != nil {
			if err != nil {
				panic(errors.Join(panicError(recovered), err))
			}
			panic(recovered)
		}
	}()
	// A failed acknowledgement can follow a successful BEGIN. This owner must
	// settle or close that same connection before it can return to the pool.
	begin := "BEGIN IMMEDIATE"
	if mode == readTransaction {
		begin = "BEGIN"
	}
	if _, err := connection.ExecContext(ctx, begin); err != nil {
		return err
	}
	access := newTransactionAccess(s, connection)
	body := func() error {
		defer access.seal()
		return apply(access)
	}
	if mode == readTransaction {
		err = scope.read(ctx, body)
	} else {
		err = body()
	}
	scope.seal()
	err = scope.include(err)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	commitIssued = true
	if _, err = connection.ExecContext(context.Background(), `COMMIT`); err != nil {
		return err
	}
	settled = true
	return nil
}

func (s *Store) IsAuthority(admission *frame.Admission, epoch uint64) bool {
	return s != nil && s.admission == admission && s.epoch == epoch
}
