package distribution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

func ownerConfig(directory string) Config {
	return Config{Directory: directory, Admission: framehost.NewAdmission(), Trust: func(context.Context, Source, Manifest, Lock) error { return nil }, Retained: func(context.Context, framevalue.Digest) (bool, error) { return false, nil }}
}

type missingCloseContext struct{ context.Context }

func TestStoreRejectsMissingCloseContextBeforeDraining(t *testing.T) {
	_, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	if err := store.Close((*missingCloseContext)(nil)); !errors.Is(err, frame.ErrInvalid) {
		t.Fatalf("missing wait context admitted: %v", err)
	}
	lease, err := store.Acquire(t.Context(), release.Digest)
	if err != nil {
		t.Fatalf("invalid close drained the store: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func closeStore(t *testing.T, store Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryHasOneOwnerAcrossConcurrentOpenAndPathAliases(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "content")
	const contenders = 12
	var group sync.WaitGroup
	stores := make(chan Store, contenders)
	failures := make(chan error, contenders)
	for range contenders {
		group.Go(func() {
			store, err := Open(ownerConfig(directory))
			if err != nil {
				failures <- err
			} else {
				stores <- store
			}
		})
	}
	group.Wait()
	close(stores)
	close(failures)
	if len(stores) != 1 {
		t.Fatalf("concurrent directory owners = %d", len(stores))
	}
	original := <-stores
	for err := range failures {
		if !errors.Is(err, framehost.ErrInUse) {
			t.Fatalf("concurrent open = %v", err)
		}
	}
	alias := filepath.Join(filepath.Dir(directory), "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := Open(ownerConfig(alias)); !errors.Is(err, framehost.ErrInUse) || duplicate.storeAuthority != nil {
		t.Fatalf("alias acquired the original inode: %v", err)
	}
	closeStore(t, original)
	closeStore(t, original)
	next, err := Open(ownerConfig(directory))
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, next)
}

func TestCloseJoinsAcceptedIOAndLeasesAfterItsOnlyWaiterCancels(t *testing.T) {
	_, _, data, _ := packageFixture(t)
	directory := t.TempDir()
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	config := ownerConfig(directory)
	config.Trust = func(context.Context, Source, Manifest, Lock) error { close(entered); <-resume; return nil }
	store, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	closing := store
	archive := filepath.Join(t.TempDir(), "package.zip")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease Lease
		err   error
	}
	installed := make(chan result, 1)
	go func() {
		lease, err := store.ImportArchiveHeld(t.Context(), archive, framevalue.Hash(data))
		installed <- result{lease, err}
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := closing.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled close wait = %v", err)
	}
	if _, err := store.Acquire(t.Context(), framevalue.Hash([]byte("unrelated"))); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("closing store admitted new claim: %v", err)
	}
	if _, err := Open(ownerConfig(directory)); !errors.Is(err, framehost.ErrInUse) {
		t.Fatalf("cancelled wait released directory lock: %v", err)
	}
	once.Do(func() { close(resume) })
	installation := <-installed
	if installation.err != nil {
		t.Fatal(installation.err)
	}
	if _, err := installation.lease.Read(t.Context(), "theme.css"); err != nil {
		t.Fatalf("original admitted lease lost its content while closing: %v", err)
	}
	select {
	case <-store.closeDone:
		t.Fatalf("close abandoned an original lease: %v", store.closeErr)
	default:
	}
	if err := installation.lease.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.closeDone:
		if store.closeErr != nil {
			t.Fatal(store.closeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("original close required another waiter after lease exit")
	}
	if _, err := store.Inspect(t.Context(), installation.lease.Snapshot().Digest); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("closed Store read content: %v", err)
	}
	next, err := Open(ownerConfig(directory))
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, next)
	if err := removeSealed(directory); err != nil {
		t.Fatal(err)
	}
}

func TestOriginalDirectoryReplacementAndParentSymlinkFenceEveryAccess(t *testing.T) {
	for _, replacement := range []string{"directory", "parent-symlink"} {
		t.Run(replacement, func(t *testing.T) {
			base := t.TempDir()
			parent := filepath.Join(base, "parent")
			directory := filepath.Join(parent, "content")
			store, err := Open(ownerConfig(directory))
			if err != nil {
				t.Fatal(err)
			}
			_, _, data, _ := packageFixture(t)
			release := installArchive(t, store, data)
			lease, err := store.Acquire(t.Context(), release.Digest)
			if err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(base, "original")
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			if replacement == "parent-symlink" {
				if err := os.Symlink(moved, parent); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "unrelated"), []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := lease.Read(t.Context(), "theme.css"); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("lease followed a replaced directory: %v", err)
			}
			if _, err := lease.Directory(); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("directory projection followed replacement: %v", err)
			}
			if _, err := store.Digests(t.Context()); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("listing followed replacement: %v", err)
			}
			if err := store.Remove(t.Context(), release.Digest); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("collector followed replacement: %v", err)
			}
			if replacement == "directory" {
				value, err := os.ReadFile(filepath.Join(directory, "unrelated"))
				if err != nil || string(value) != "untouched" {
					t.Fatal("replacement directory was mutated")
				}
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			closeStore(t, store)
			if err := removeSealed(moved); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryReclaimsPrivateOperationsOnlyAfterExclusiveOwnership(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(ownerConfig(directory))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".stage-interrupted", ".retiring-interrupted"} {
		child := filepath.Join(directory, name)
		if err := os.MkdirAll(child, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(child, "retained"), []byte("private incomplete operation"), 0400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(child, 0500); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(ownerConfig(directory)); !errors.Is(err, framehost.ErrInUse) {
		t.Fatalf("second owner performed recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, ".stage-interrupted")); err != nil {
		t.Fatal("unowned recovery removed live staging")
	}
	closeStore(t, store)
	public := framevalue.Hash([]byte("public release")).String()
	if err := os.Mkdir(filepath.Join(directory, public), 0700); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(ownerConfig(directory))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".stage-interrupted", ".retiring-interrupted"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("abandoned private operation survived recovery: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, public)); err != nil {
		t.Fatal("recovery touched public release namespace")
	}
	closeStore(t, resumed)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "marker"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, ".stage-malicious")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ownerConfig(directory)); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("recovery followed a staging symlink: %v", err)
	}
	if value, err := os.ReadFile(filepath.Join(outside, "marker")); err != nil || string(value) != "outside" {
		t.Fatal("recovery modified an outside directory")
	}
	if err := os.Remove(filepath.Join(directory, ".stage-malicious")); err != nil {
		t.Fatal(err)
	}
	final, err := Open(ownerConfig(directory))
	if err != nil {
		t.Fatalf("failed recovery retained the directory lock: %v", err)
	}
	closeStore(t, final)
}
