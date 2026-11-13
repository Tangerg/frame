package sqlite_test

import (
	"context"
	"database/sql"

	"github.com/Tangerg/frame/sqlite"
)

type standaloneQuery struct {
	database  *sqlite.Database
	ctx       context.Context
	statement string
	args      []any
}

func databaseQuery(database *sqlite.Database, ctx context.Context, statement string, args ...any) standaloneQuery {
	return standaloneQuery{database, ctx, statement, args}
}
func (q standaloneQuery) Scan(dest ...any) error {
	return q.database.Read(q.ctx, func(reader sqlite.Reader) error {
		return reader.QueryRowContext(q.ctx, q.statement, q.args...).Scan(dest...)
	})
}
func execDatabase(database *sqlite.Database, ctx context.Context, statement string, args ...any) (result sql.Result, err error) {
	err = sqlite.RunInTx(ctx, database, func(ctx context.Context) error {
		var err error
		result, err = sqlite.Connection(ctx, database).ExecContext(ctx, statement, args...)
		return err
	})
	return result, err
}
