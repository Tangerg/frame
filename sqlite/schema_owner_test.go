package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
	modernc "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func noSchemaReferences(context.Context, sqlite.Reader, framehost.InstallationID) ([]framehost.Dependency, error) {
	return nil, nil
}

func schemaOwnerStore(t *testing.T, owners ...sqlite.OwnerSchema) (sqlite.Store, framehost.Admission) {
	t.Helper()
	admission := framehost.NewAdmission()
	store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "owners.db"), Admission: admission, Owners: owners})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, admission
}

func requireSchemaDenied(t *testing.T, err error) {
	t.Helper()
	var denied *modernc.Error
	if !errors.As(err, &denied) || denied.Code() != sqlite3.SQLITE_AUTH {
		t.Fatalf("expected schema authority refusal, got %v", err)
	}
}

func TestBusinessOwnerCannotEraseExecutionProtection(t *testing.T) {
	store, admission := schemaOwnerStore(t, sqlite.OwnerSchema{ID: "documents", References: noSchemaReferences})
	owner, err := store.Owner("documents")
	if err != nil {
		t.Fatal(err)
	}
	source := framehost.UnitRef{InstallationID: "provider", Release: value.Hash(nil), UnitID: "main"}
	binding := framehost.BindingSnapshot{FormatVersion: 1, Roots: []framehost.ExportRef{}, UnitRoots: []framehost.UnitRef{source}, Units: []framehost.UnitBinding{{Source: source, Configuration: value.Hash(nil)}}}
	if err := store.RecordAttempt(t.Context(), sqlite.Attempt{ID: "running", Epoch: store.Epoch(), Source: source, Binding: binding, Realm: "realm", Profile: framehost.LocalProcess}); err != nil {
		t.Fatal(err)
	}
	err = owner.Update(t.Context(), func(ctx context.Context, tx sqlite.Executor) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM frame_attempts`)
		return err
	})
	requireSchemaDenied(t, err)
	dependencies, err := store.ExecutionDependencies(t.Context())
	if err != nil || len(dependencies) != 1 || admission.RecoveryError() != nil {
		t.Fatalf("refusal changed original protection: dependencies=%v error=%v recovery=%v", dependencies, err, admission.RecoveryError())
	}
}

func TestBusinessOwnerCannotReopenSettledSubmission(t *testing.T) {
	store, _ := schemaOwnerStore(t, sqlite.OwnerSchema{ID: "documents", Statements: []string{`CREATE TABLE documents (id TEXT)`}, References: noSchemaReferences})
	owner, err := store.Owner("documents")
	if err != nil {
		t.Fatal(err)
	}
	request := []byte("create-document")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	mutate := func(ctx context.Context, tx sqlite.Executor) ([]byte, error) {
		runs++
		_, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES ('original')`)
		return []byte("evidence"), err
	}
	if _, err := owner.Commit(t.Context(), id, request, mutate, nil); err != nil {
		t.Fatal(err)
	}
	err = owner.Update(t.Context(), func(ctx context.Context, tx sqlite.Executor) error {
		_, err := tx.ExecContext(ctx, `UPDATE frame_submissions SET state='prepared', evidence=NULL`)
		return err
	})
	requireSchemaDenied(t, err)
	settlement, err := owner.Commit(t.Context(), id, request, mutate, nil)
	if err != nil || settlement.State != framehost.CommitSucceeded || runs != 1 {
		t.Fatalf("original conclusion changed: settlement=%+v runs=%d error=%v", settlement, runs, err)
	}
}

func TestSchemaGrantCoversViewsTriggersAndCascades(t *testing.T) {
	store, _ := schemaOwnerStore(t,
		sqlite.OwnerSchema{ID: "a", References: noSchemaReferences, Statements: []string{
			`CREATE TABLE parents (id INTEGER PRIMARY KEY)`,
			`CREATE TABLE documents (id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`,
			`CREATE VIEW document_view AS SELECT id,label FROM documents`,
			`CREATE TRIGGER document_insert INSTEAD OF INSERT ON document_view BEGIN INSERT INTO documents(label) VALUES(NEW.label); END`,
			`CREATE TRIGGER frame_attempts AFTER DELETE ON documents BEGIN DELETE FROM frame_attempts; END`,
			`CREATE VIEW protected_attempts AS SELECT id FROM frame_attempts`,
			`CREATE TRIGGER attempt_delete INSTEAD OF DELETE ON protected_attempts BEGIN DELETE FROM frame_attempts; END`,
			`INSERT INTO parents VALUES(1)`,
		}},
		sqlite.OwnerSchema{ID: "b", References: noSchemaReferences, Statements: []string{
			`CREATE TABLE foreign_rows (id INTEGER PRIMARY KEY AUTOINCREMENT, parent INTEGER REFERENCES parents(id) ON DELETE CASCADE)`,
			`INSERT INTO foreign_rows(parent) VALUES(1)`,
		}},
	)
	owner, err := store.Owner("a")
	if err != nil {
		t.Fatal(err)
	}
	var escaped sqlite.Executor
	if err := owner.Update(t.Context(), func(ctx context.Context, tx sqlite.Executor) error {
		escaped = tx
		_, err := tx.ExecContext(ctx, `INSERT INTO DOCUMENT_VIEW(label) VALUES('original')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := escaped.ExecContext(t.Context(), `DELETE FROM frame_attempts`); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("schema capability outlived its grant: %v", err)
	}
	for _, statement := range []string{
		`DELETE FROM documents`,
		`DELETE FROM protected_attempts`,
		`DELETE FROM parents`,
		`INSERT INTO foreign_rows(parent) VALUES(1)`,
		`DELETE FROM sqlite_sequence`,
		`CREATE TABLE extra (id TEXT)`,
		`ALTER TABLE documents ADD COLUMN extra TEXT`,
		`DROP TABLE documents`,
	} {
		t.Run(statement, func(t *testing.T) {
			err := owner.Update(t.Context(), func(ctx context.Context, tx sqlite.Executor) error {
				_, err := tx.ExecContext(ctx, statement)
				return err
			})
			requireSchemaDenied(t, err)
		})
	}
	err = owner.Update(t.Context(), func(ctx context.Context, tx sqlite.Executor) error {
		rows, err := tx.QueryContext(ctx, `DELETE FROM frame_attempts RETURNING id`)
		if rows != nil {
			err = errors.Join(err, rows.Close())
		}
		return err
	})
	requireSchemaDenied(t, err)
	if err := store.Read(t.Context(), func(tx sqlite.Reader) error {
		var documents, foreign int
		if err := tx.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM documents),(SELECT count(*) FROM foreign_rows)`).Scan(&documents, &foreign); err != nil {
			return err
		}
		if documents != 1 || foreign != 1 {
			t.Errorf("unauthorized effects survived: documents=%d foreign=%d", documents, foreign)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaDeclarationCannotChangeAnotherOwnersFacts(t *testing.T) {
	for _, statement := range []string{
		`DELETE FROM frame_attempts`,
		`INSERT INTO frame_owners(id) VALUES('extra')`,
		`DROP TABLE frame_attempts`,
		`CREATE TRIGGER borrowed AFTER INSERT ON frame_attempts BEGIN SELECT 1; END`,
		`CREATE TABLE local (id TEXT); DELETE FROM frame_submissions`,
		`CREATE VIRTUAL TABLE physical_pages USING sqlite_dbpage`,
		`CREATE VIRTUAL TABLE searchable_documents USING fts5(body)`,
	} {
		t.Run(statement, func(t *testing.T) {
			store, err := sqlite.Open(t.Context(), sqlite.Config{
				Path: filepath.Join(t.TempDir(), "invalid.db"), Admission: framehost.NewAdmission(),
				Owners: []sqlite.OwnerSchema{{ID: "documents", Statements: []string{statement}, References: noSchemaReferences}},
			})
			_ = store.Close()
			requireSchemaDenied(t, err)
		})
	}
}

func TestInstallationProjectionUsesItsOriginalSchemaOwner(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		name := "foreign state"
		if allowed {
			name = "own projection"
		}
		t.Run(name, func(t *testing.T) {
			declaration := sqlite.OwnerSchema{ID: "documents", Statements: []string{`CREATE TABLE projections (id TEXT PRIMARY KEY)`}, References: noSchemaReferences}
			declaration.ProjectInstallation = func(ctx context.Context, tx sqlite.Executor, change framehost.InstallationChange) error {
				if !allowed {
					_, err := tx.ExecContext(ctx, `DELETE FROM frame_installations`)
					return err
				}
				_, err := tx.ExecContext(ctx, `INSERT INTO projections VALUES(?)`, change.After.ID)
				return err
			}
			store, admission := schemaOwnerStore(t, declaration)
			host, err := framehost.New(t.Context(), framehost.Config{Admission: admission, Installations: store})
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close(context.Background())
			plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash(nil)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = host.Install(t.Context(), framehost.InstallationConfig{ID: "plugin", Plugin: plugin})
			if !allowed {
				requireSchemaDenied(t, err)
			} else if err != nil {
				t.Fatal(err)
			}
			records, err := store.Installations(t.Context())
			if err != nil || (len(records) == 1) != allowed {
				t.Fatalf("projection escaped original installation transaction: records=%v error=%v", records, err)
			}
		})
	}
}
