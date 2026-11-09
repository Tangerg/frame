package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirectoryOwnsTheAuthorityLock(t *testing.T) {
	directory := t.TempDir()
	server, err := Open(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	formerLock := filepath.Join(directory, "authority.lock")
	if _, err := os.Stat(formerLock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authority still depends on an unlinkable lock file: %v", err)
	}
	if err := os.WriteFile(formerLock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(formerLock); err != nil {
		t.Fatal(err)
	}
	if replacement, err := Open(Config{Directory: directory}); err == nil {
		_ = replacement.Close()
		t.Fatal("unlinking a directory entry created a second owner")
	}
}

func TestRenamedAuthorityRetainsOriginalJournalAndCleanup(t *testing.T) {
	parent := t.TempDir()
	directory, retired := filepath.Join(parent, "current"), filepath.Join(parent, "original")
	server, err := Open(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.Fence(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	request := launchFixture(t, server, "original-child", 1, t.TempDir())
	if _, err := server.Launch(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, retired); err != nil {
		t.Fatal(err)
	}
	if conflicting, err := Open(Config{Directory: retired}); err == nil {
		_ = conflicting.Close()
		t.Fatal("renaming the directory detached its original lock")
	}
	replacement, err := Open(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := replacement.Fence(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if err := server.Fence(t.Context(), 2); !errors.Is(err, ErrFenced) {
		t.Fatalf("original server accepted replacement pathname: %v", err)
	}
	exit, err := server.CloseAttempt(t.Context(), request.AttemptRef)
	if err != nil || exit.State != Exited {
		t.Fatalf("original child lost cleanup after directory rename: %+v %v", exit, err)
	}
	receipt, err := exit.ExitReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.AcknowledgeExit(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if observed, err := server.Inspect(t.Context(), request.AttemptID); err != nil || observed.ProofID != exit.ProofID {
		t.Fatalf("original receipt was not retained: %+v %v", observed, err)
	}
	if _, err := replacement.Inspect(t.Context(), request.AttemptID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("original cleanup wrote into replacement authority: %v", err)
	}
	journal, err := loadJournal(replacement.directory, replacement.config.MaxAttempts)
	if err != nil || journal.Epoch != 10 || len(journal.Attempts) != 0 {
		t.Fatalf("old authority overwrote replacement journal: %+v %v", journal, err)
	}
	entries, err := os.ReadDir(retired)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != journalFilename && entry.Name() != receiptDirectory {
			t.Fatalf("original cleanup left a snapshot or temporary record: %s", entry.Name())
		}
	}
}
