package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/sqlite"
)

func TestRunInTxCancellationDoesNotDependOnNesting(t *testing.T) {
	callbackFailure := errors.New("callback failed")
	for _, test := range []struct {
		name        string
		nested      bool
		callbackErr error
	}{
		{name: "independent success"},
		{name: "nested success", nested: true},
		{name: "independent failure", callbackErr: callbackFailure},
		{name: "nested failure", nested: true, callbackErr: callbackFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, err := sqlite.OpenSchema(t.Context(), filepath.Join(t.TempDir(), "owner.db"), []string{`CREATE TABLE entries (id INTEGER)`})
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			cause := errors.New("operation canceled")
			run := func(parent context.Context) error {
				ctx, cancel := context.WithCancelCause(parent)
				defer cancel(nil)
				return sqlite.RunInTx(ctx, database, func(ctx context.Context) error {
					if _, err := sqlite.Connection(ctx, database).ExecContext(ctx, `INSERT INTO entries VALUES (1)`); err != nil {
						return err
					}
					cancel(cause)
					return test.callbackErr
				})
			}
			if test.nested {
				err = sqlite.RunInTx(t.Context(), database, run)
			} else {
				err = run(t.Context())
			}
			if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || test.callbackErr != nil && !errors.Is(err, test.callbackErr) {
				t.Fatalf("transaction lost its completion causes: %v", err)
			}
			var count int
			if err := database.Read(t.Context(), func(reader sqlite.Reader) error {
				return reader.QueryRowContext(t.Context(), `SELECT count(*) FROM entries`).Scan(&count)
			}); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("canceled transaction committed %d rows", count)
			}
		})
	}
}
