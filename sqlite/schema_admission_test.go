package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tangerg/frame/sqlite"
)

func TestWALAdmissionKeepsCallerCancellationBeforeSchemaInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	for _, statement := range seededSchema {
		if _, err := blocker.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	read, err := blocker.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	var value string
	if err := read.QueryRowContext(t.Context(), `SELECT value FROM identity WHERE id=1`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("wal admission caller stopped")
	ctx, cancel := context.WithTimeoutCause(t.Context(), 25*time.Millisecond, cause)
	defer cancel()
	type result struct {
		db  *sql.DB
		err error
	}
	settled := make(chan result, 1)
	go func() {
		db, err := sqlite.OpenSchema(ctx, path, seededSchema)
		settled <- result{db, err}
	}()
	var opened result
	// The error category alone cannot prove cancellation while the peer keeps
	// its lock: implicit driver setup can return the same error after waiting.
	select {
	case opened = <-settled:
	case <-time.After(time.Second):
		_ = read.Rollback()
		opened = <-settled
		if opened.db != nil {
			_ = opened.db.Close()
		}
		t.Fatalf("WAL admission ignored cancellation until the read owner released: %v", opened.err)
	}
	db, err := opened.db, opened.err
	if db != nil {
		_ = db.Close()
		t.Fatal("WAL conversion crossed another owner's read transaction")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WAL admission did not wait with caller cancellation: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("WAL admission lost its caller's cancellation cause: %v", err)
	}
	if err := read.Rollback(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.OpenSchema(t.Context(), path, seededSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("admitted journal mode = %q, %v", mode, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT value FROM identity WHERE id=1`).Scan(&value); err != nil || value != "original" {
		t.Fatalf("original owner fact = %q, %v", value, err)
	}
}

func TestSchemaAdmissionSupportsExplicitMemoryDatabase(t *testing.T) {
	db, err := sqlite.OpenSchema(t.Context(), ":memory:", seededSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRowContext(t.Context(), `SELECT value FROM identity WHERE id=1`).Scan(&value); err != nil || value != "original" {
		t.Fatalf("memory owner fact = %q, %v", value, err)
	}
}

var seededSchema = []string{
	`CREATE TABLE IF NOT EXISTS identity (id INTEGER PRIMARY KEY, value TEXT NOT NULL)`,
	`INSERT OR IGNORE INTO identity VALUES (1, 'original')`,
}

func TestCurrentSchemaAdmissionDoesNotAcquireAnotherOwnersWriteSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	owner, err := sqlite.OpenSchema(t.Context(), path, seededSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	write, err := owner.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Rollback()
	if _, err := write.ExecContext(t.Context(), `UPDATE identity SET value='uncommitted' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	reader, err := sqlite.OpenSchema(t.Context(), path, seededSchema)
	if err != nil {
		t.Fatalf("current schema admission required a competing write-set: %v", err)
	}
	defer reader.Close()
	var value string
	if err := reader.QueryRowContext(t.Context(), `SELECT value FROM identity WHERE id=1`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "original" {
		t.Fatalf("schema admission read an uncommitted owner fact: %q", value)
	}
}

func TestCurrentSchemaAdmissionNeverRecreatesAnOwnersMissingFact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	owner, err := sqlite.OpenSchema(t.Context(), path, seededSchema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(t.Context(), `DELETE FROM identity`); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := sqlite.OpenSchema(t.Context(), path, seededSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err := reader.QueryRowContext(t.Context(), `SELECT count(*) FROM identity`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("schema admission recreated an owner fact from initialization SQL")
	}
}

func TestConcurrentSchemaInitializationAdmitsOneOwnerFact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			db, err := sqlite.OpenSchema(t.Context(), path, seededSchema)
			if err != nil {
				results <- err
				return
			}
			var count int
			err = db.QueryRowContext(t.Context(), `SELECT count(*) FROM identity`).Scan(&count)
			if err == nil && count != 1 {
				err = fmt.Errorf("initialization facts = %d, want one", count)
			}
			results <- errors.Join(err, db.Close())
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}
