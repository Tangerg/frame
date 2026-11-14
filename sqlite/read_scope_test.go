package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame/sqlite"
)

func readScopeDatabase(t *testing.T) (sqlite.Database, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "read-scopes.db")
	database, err := sqlite.OpenSchema(t.Context(), path, []string{`CREATE TABLE facts (value TEXT NOT NULL)`})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	return database, path
}

func TestOverlappingReadScopesKeepTheirOriginalConnectionReadOnly(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "siblings"
		if nested {
			name = "nested-reader-outlives-parent"
		}
		t.Run(name, func(t *testing.T) {
			database, _ := readScopeDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var writeFailure error
			err := sqlite.RunInTx(ctx, database, func(writer context.Context) error {
				firstContext := make(chan context.Context, 1)
				firstDone, secondDone := make(chan error, 1), make(chan error, 1)
				releaseFirst, firstReleased := make(chan struct{}), make(chan struct{})
				allowFirstReturn := sync.OnceFunc(func() { close(releaseFirst) })
				allowSecondContinue := sync.OnceFunc(func() { close(firstReleased) })
				defer allowFirstReturn()
				defer allowSecondContinue()
				secondEntered := make(chan struct{})
				go func() {
					firstDone <- sqlite.RunReadSnapshot(writer, database, func(read context.Context) error {
						firstContext <- read
						select {
						case <-releaseFirst:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					})
				}()
				var parent context.Context
				select {
				case parent = <-firstContext:
				case err := <-firstDone:
					return errors.Join(errors.New("first read scope was not admitted"), err)
				case <-ctx.Done():
					return ctx.Err()
				}
				if !nested {
					parent = writer
				}
				go func() {
					secondDone <- sqlite.RunReadSnapshot(parent, database, func(read context.Context) error {
						close(secondEntered)
						select {
						case <-firstReleased:
						case <-ctx.Done():
							return ctx.Err()
						}
						_, writeFailure = sqlite.Connection(read, database).ExecContext(read, `INSERT INTO facts VALUES('read scope must not write')`)
						return nil
					})
				}()
				select {
				case <-secondEntered:
				case err := <-secondDone:
					return errors.Join(errors.New("second read scope was not admitted"), err)
				case <-ctx.Done():
					return errors.New("read scope blocked another reader or deadlocked a nested scope")
				}
				allowFirstReturn()
				firstErr := <-firstDone
				allowSecondContinue()
				if err := errors.Join(firstErr, <-secondDone); err != nil {
					return err
				}
				_, err := sqlite.Connection(writer, database).ExecContext(writer, `INSERT INTO facts VALUES('original writer')`)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if writeFailure == nil {
				t.Fatal("an earlier scope restored write mode while another reader was still admitted")
			}
			var count int
			if err := databaseQuery(database, ctx, `SELECT count(*) FROM facts`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("read scopes changed the original writer's facts: count=%d error=%v", count, err)
			}
		})
	}
}

func TestTransactionSealsAndJoinsAcceptedReadScopesBeforeCommit(t *testing.T) {
	database, path := readScopeDatabase(t)
	observer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	ownerReturned := make(chan struct{})
	defer func() { unblock(); <-ownerReturned }()
	originalContext := make(chan context.Context, 1)
	readDone, committed := make(chan error, 1), make(chan error, 1)
	go func() {
		defer close(ownerReturned)
		committed <- sqlite.RunInTx(ctx, database, func(writer context.Context) error {
			originalContext <- writer
			if _, err := sqlite.Connection(writer, database).ExecContext(writer, `INSERT INTO facts VALUES('pending commit')`); err != nil {
				return err
			}
			go func() {
				readDone <- sqlite.RunReadSnapshot(writer, database, func(context.Context) error {
					close(entered)
					<-release
					return nil
				})
			}()
			select {
			case <-entered:
				return nil
			case err := <-readDone:
				return errors.Join(errors.New("read scope was not admitted"), err)
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var original context.Context
	select {
	case original = <-originalContext:
	case <-ownerReturned:
		t.Fatalf("transaction did not start: %v", <-committed)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-entered:
	case <-ownerReturned:
		t.Fatalf("read scope did not start: %v", <-committed)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for {
		err := sqlite.RunReadSnapshot(original, database, func(context.Context) error { return nil })
		if errors.Is(err, sql.ErrTxDone) {
			break
		}
		if err != nil {
			t.Fatalf("read admission did not seal: %v", err)
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("transaction did not seal its read scopes: %v", err)
		}
		runtime.Gosched()
	}
	select {
	case err := <-committed:
		t.Fatalf("transaction returned while its accepted reader remained active: %v", err)
	default:
	}
	var visible int
	if err := observer.QueryRowContext(ctx, `SELECT count(*) FROM facts`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("original COMMIT overtook its accepted read scope: count=%d error=%v", visible, err)
	}
	unblock()
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	if err := observer.QueryRowContext(ctx, `SELECT count(*) FROM facts`).Scan(&visible); err != nil || visible != 1 {
		t.Fatalf("joined reader lost its original commit: count=%d error=%v", visible, err)
	}
	if err := sqlite.RunReadSnapshot(original, database, func(context.Context) error {
		t.Fatal("closed transaction admitted a new read scope")
		return nil
	}); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("closed read admission: %v", err)
	}
}
