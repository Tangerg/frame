// Package sqlite owns the single-authority durable submission and execution
// boundaries. Applications retain ownership of their business tables.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
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
	defaultMaxRequestBytes      = 1 << 20
	defaultMaxSubmissions       = 4096
	defaultMaxRealmHistoryBytes = 256 << 20
	// Frame's own submission owners live in this namespace; application
	// owners cannot claim it.
	reservedOwnerPrefix = "frame."
	// Write-ahead logging lets readers proceed beside the single writer.
	walJournalMode      = "wal"
	installationOwnerID = reservedOwnerPrefix + "installations"
	synchronousFull     = 2
	pragmaEnabled       = 1
	pragmaDisabled      = 0
)

type Reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *Row
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
	References func(context.Context, Reader, framehost.InstallationID) ([]framehost.Dependency, error)
	// ProjectInstallation writes this owner's projection in the original
	// installation transaction, without authority over Frame's state.
	ProjectInstallation func(context.Context, Executor, framehost.InstallationChange) error
}

type Config struct {
	Path                   string
	Admission              framehost.Admission
	Owners                 []OwnerSchema
	MaxRequestBytes        int
	MaxSubmissionsPerOwner int
	MaxRealmHistoryBytes   int64
}

// Store is a value handle. Passing it to a Host or driver captures the original
// authority rather than a caller-owned variable that can later be rebound.
type Store struct{ *storeAuthority }

// Issued capabilities retain this authority, never a caller-owned Store handle.
type storeAuthority struct {
	*databaseOwner
	admission            framehost.Admission
	id                   string
	epoch                uint64
	owners               map[string]Owner
	control              Owner
	maxRequestBytes      int
	maxSubmissions       int
	maxRealmHistoryBytes int64
}

// Owner is a value capability for an admitted schema. Copies keep that original
// authority even when a caller reassigns its handle after registering it.
type Owner struct {
	store               *storeAuthority
	id                  string
	references          func(context.Context, Reader, framehost.InstallationID) ([]framehost.Dependency, error)
	schema              schemaWriteSet
	projectInstallation func(context.Context, Executor, framehost.InstallationChange) error
}

const schema = `
CREATE TABLE frame_meta (singleton INTEGER PRIMARY KEY CHECK(singleton=1),store_id TEXT NOT NULL,schema_digest TEXT NOT NULL,epoch INTEGER NOT NULL CHECK(epoch>0));
CREATE TABLE frame_owners (id TEXT PRIMARY KEY,next_sequence INTEGER NOT NULL DEFAULT 0,compacted_through INTEGER NOT NULL DEFAULT 0 CHECK(compacted_through<=next_sequence));
CREATE TABLE frame_submissions (owner_id TEXT NOT NULL REFERENCES frame_owners(id),sequence INTEGER NOT NULL,digest TEXT NOT NULL,request BLOB NOT NULL,state TEXT NOT NULL CHECK(state IN ('` + string(submissionPrepared) + `','` + string(submissionCommitted) + `','` + string(submissionRejected) + `','` + string(submissionCommittedSynchronized) + `','` + string(submissionRejectedSynchronized) + `')),evidence BLOB,CHECK(state IN ('` + string(submissionCommitted) + `','` + string(submissionCommittedSynchronized) + `') OR evidence IS NULL),PRIMARY KEY(owner_id,sequence));
CREATE TABLE frame_evidence_references (owner_id TEXT NOT NULL,sequence INTEGER NOT NULL,holder TEXT NOT NULL,PRIMARY KEY(owner_id,sequence,holder),FOREIGN KEY(owner_id,sequence) REFERENCES frame_submissions(owner_id,sequence));
CREATE TABLE frame_attempts (id TEXT PRIMARY KEY,request BLOB NOT NULL,request_digest TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('` + string(AttemptApproved) + `','` + string(AttemptHandoff) + `','` + string(AttemptReleased) + `')),proof BLOB);
CREATE INDEX frame_attempts_by_state ON frame_attempts(state,id);
CREATE TABLE frame_installations (id TEXT PRIMARY KEY,record BLOB NOT NULL);
CREATE TABLE frame_use_approvals (id TEXT PRIMARY KEY,request BLOB NOT NULL,execution_id TEXT GENERATED ALWAYS AS (json_extract(CAST(request AS TEXT),'$.ExecutionID')) VIRTUAL,request_digest TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('` + string(useApproved) + `','` + string(useReleased) + `')),proof BLOB);
CREATE INDEX frame_use_approvals_by_state ON frame_use_approvals(state,id);
CREATE INDEX frame_uses_by_execution ON frame_use_approvals(execution_id,id);
CREATE TABLE frame_realms (id TEXT NOT NULL,epoch TEXT NOT NULL,operation_id TEXT NOT NULL UNIQUE,resume_verifier TEXT NOT NULL,history_bytes INTEGER GENERATED ALWAYS AS (length(CAST(id AS BLOB))+length(CAST(epoch AS BLOB))+length(CAST(operation_id AS BLOB))+length(CAST(resume_verifier AS BLOB))) VIRTUAL,PRIMARY KEY(id,epoch));
CREATE TABLE frame_exit_proofs (attempt_id TEXT NOT NULL REFERENCES frame_attempts(id),scope TEXT NOT NULL,proof_id TEXT NOT NULL,evidence BLOB NOT NULL,PRIMARY KEY(attempt_id,proof_id),UNIQUE(attempt_id,scope));
` + realmReceiptSchema

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

func readSchemaShape(ctx context.Context, executor interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (framevalue.Digest, error) {
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

func (s Store) ID() string {
	if s.storeAuthority == nil {
		return ""
	}
	return s.id
}

func (s Store) Epoch() uint64 {
	if s.storeAuthority == nil {
		return 0
	}
	return s.epoch
}

func (s *storeAuthority) Owner(id string) (Owner, error) {
	if s == nil {
		return Owner{}, frame.ErrInvalid
	}
	owner, ok := s.owners[id]
	if !ok {
		return Owner{}, fmt.Errorf("sqlite: required owner %q is unavailable", id)
	}
	return owner, nil
}

func (o Owner) ID() string { return o.id }

// Held starts a new read after admission. It must not be called from an owner
// mutation callback; that callback already has the current transaction.
func (o Owner) Held(ctx context.Context, installation framehost.InstallationID) ([]framehost.Dependency, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	var dependencies []framehost.Dependency
	err := o.store.Read(ctx, func(tx Reader) error {
		var err error
		dependencies, err = o.references(ctx, tx, installation)
		return err
	})
	return dependencies, err
}

// fenceUnlessRefused requires recovery after a protection write fails for any
// reason other than a known refusal: the Store can no longer vouch for the
// durable state that write meant to change.
func (s *storeAuthority) fenceUnlessRefused(err error) error {
	if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, frame.ErrClosed) && !errors.Is(err, sql.ErrNoRows) {
		s.admission.RequireRecovery(err)
	}
	return err
}

func (s Store) IsAuthority(admission framehost.Admission, epoch uint64) bool {
	return s.storeAuthority != nil && s.admission == admission && s.epoch == epoch
}

// Recovery and diagnostics may read without reopening admission. Mutations
// require an Owner so they cannot bypass the recovery barrier.
func (s *storeAuthority) Read(ctx context.Context, query func(Reader) error) error {
	if query == nil {
		return errors.New("sqlite: query is required")
	}
	return s.transact(ctx, readTransaction, func(tx Executor) error { return query(readAccess{access: tx}) }, nil)
}
