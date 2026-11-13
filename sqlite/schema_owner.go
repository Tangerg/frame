package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	framevalue "github.com/Tangerg/frame/value"
	modernc "modernc.org/sqlite"
)

const (
	mainDatabaseName     = "main"
	sqliteMasterName     = "sqlite_master"
	sqliteSchemaName     = "sqlite_schema"
	sqliteSequenceName   = "sqlite_sequence"
	sqliteReservedPrefix = "sqlite_"
)

// The grant is derived from the original schema declaration, never supplied as
// a second list of tables. Once Open returns, it cannot acquire new relations.
type schemaWriteSet struct {
	relations map[string]struct{}
}

func (s schemaWriteSet) contains(name string) bool {
	_, ok := s.relations[sqliteIdentifier(name)]
	return ok
}

func (s schemaWriteSet) owns(name, database string) bool {
	return sqliteIdentifier(database) == mainDatabaseName && s.contains(name)
}

func (s schemaWriteSet) authorize(action modernc.AuthorizerActionCode, first, second, database, source string) modernc.AuthorizerReturnCode {
	switch action {
	case modernc.AuthInsert, modernc.AuthUpdate, modernc.AuthDelete:
		if s.owns(first, database) {
			return modernc.AuthorizerOK
		}
		return modernc.AuthorizerDeny
	default:
		return readStatements.authorize(action, first, second, database, source)
	}
}

// SQLite folds ASCII identifiers; Unicode case folding would grant authority
// over distinct names that SQLite itself does not consider equivalent.
func sqliteIdentifier(name string) string {
	return strings.Map(func(character rune) rune {
		if character >= 'A' && character <= 'Z' {
			return character + ('a' - 'A')
		}
		return character
	}, name)
}

type schemaAdmission struct {
	existing schemaWriteSet
	writes   schemaWriteSet
	indexes  map[string]struct{}
}

func (a *schemaAdmission) authorize(action modernc.AuthorizerActionCode, first, second, database, source string) modernc.AuthorizerReturnCode {
	if sqliteIdentifier(database) == mainDatabaseName {
		switch action {
		case modernc.AuthCreateTable, modernc.AuthCreateView:
			// Virtual modules can write outside their declared relation, so
			// only SQLite-owned tables and views can acquire this grant.
			return a.admitRelation(first)
		case modernc.AuthCreateIndex, modernc.AuthCreateTrigger:
			if a.writes.contains(second) {
				if action == modernc.AuthCreateIndex {
					a.indexes[sqliteIdentifier(first)] = struct{}{}
				}
				return modernc.AuthorizerOK
			}
		case modernc.AuthReindex:
			if _, ok := a.indexes[sqliteIdentifier(first)]; ok {
				return modernc.AuthorizerOK
			}
		case modernc.AuthInsert, modernc.AuthUpdate, modernc.AuthDelete:
			// SQLite's DDL writes its own catalog. Direct catalog DML remains
			// prohibited by SQLite; this grant cannot enable writable_schema.
			name := sqliteIdentifier(first)
			if name == sqliteMasterName || name == sqliteSchemaName {
				return modernc.AuthorizerOK
			}
		}
	}
	return a.writes.authorize(action, first, second, database, source)
}

func (a *schemaAdmission) admitRelation(name string) modernc.AuthorizerReturnCode {
	name = sqliteIdentifier(name)
	if name == sqliteSequenceName {
		// AUTOINCREMENT owns this engine-maintained projection; applications
		// must not gain authority to reset another table's sequence.
		return modernc.AuthorizerOK
	}
	if strings.HasPrefix(name, sqliteReservedPrefix) || a.existing.contains(name) {
		return modernc.AuthorizerDeny
	}
	a.writes.relations[name] = struct{}{}
	return modernc.AuthorizerOK
}

func (s *storeAuthority) admitSchema(ctx context.Context, owners []OwnerSchema) (shape framevalue.Digest, err error) {
	database, err := openDatabase(":memory:")
	if err != nil {
		return shape, err
	}
	defer func() { err = errors.Join(err, database.Close()) }()
	connection, err := database.Conn(ctx)
	if err != nil {
		return shape, err
	}
	defer func() { err = errors.Join(err, connection.Close()) }()
	if _, err := connection.ExecContext(ctx, schema); err != nil {
		return shape, err
	}
	existing, err := readSchemaRelations(ctx, connection)
	if err != nil {
		return shape, err
	}
	s.control.schema = schemaWriteSet{relations: make(map[string]struct{}, len(existing.relations))}
	for name := range existing.relations {
		s.control.schema.relations[name] = struct{}{}
	}
	for _, declaration := range owners {
		admission := &schemaAdmission{existing: existing, writes: schemaWriteSet{relations: make(map[string]struct{})}, indexes: make(map[string]struct{})}
		if err := admitSchemaStatements(ctx, connection, admission, declaration.Statements); err != nil {
			return shape, fmt.Errorf("sqlite: owner %q schema: %w", declaration.ID, err)
		}
		owner := s.owners[declaration.ID]
		owner.schema = admission.writes
		s.owners[declaration.ID] = owner
		for name := range admission.writes.relations {
			existing.relations[name] = struct{}{}
		}
	}
	return readSchemaShape(ctx, connection)
}

func admitSchemaStatements(ctx context.Context, connection *sql.Conn, admission *schemaAdmission, statements []string) (err error) {
	register := func(policy modernc.AuthorizerFn) error {
		return connection.Raw(func(driver any) error {
			return driver.(*cursorConnection).RegisterAuthorizer(policy)
		})
	}
	defer func() { err = errors.Join(err, register(nil)) }()
	if err := register(admission.authorize); err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := connection.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func readSchemaRelations(ctx context.Context, connection *sql.Conn) (schemaWriteSet, error) {
	rows, err := connection.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name NOT GLOB 'sqlite_*'`)
	if err != nil {
		return schemaWriteSet{}, err
	}
	defer rows.Close()
	result := schemaWriteSet{relations: make(map[string]struct{})}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return schemaWriteSet{}, err
		}
		result.relations[sqliteIdentifier(name)] = struct{}{}
	}
	return result, rows.Err()
}

func (o Owner) withinSchema(tx Executor, apply func(Executor) error) error {
	access, ok := tx.(*transactionAccess)
	if !ok {
		return errors.New("sqlite: schema grant requires its original transaction")
	}
	return access.withPolicy(o.schema.authorize, apply)
}
