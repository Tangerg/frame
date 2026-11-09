package sqlite

import (
	"context"
)

type standaloneQuery struct {
	database  *Database
	ctx       context.Context
	statement string
	args      []any
}

func databaseQuery(database *Database, ctx context.Context, statement string, args ...any) standaloneQuery {
	return standaloneQuery{database, ctx, statement, args}
}
func (q standaloneQuery) Scan(dest ...any) error {
	return q.database.Read(q.ctx, func(reader Reader) error { return reader.QueryRowContext(q.ctx, q.statement, q.args...).Scan(dest...) })
}
