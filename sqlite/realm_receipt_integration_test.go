package sqlite_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/sqlite"
)

type receiptModules struct{}

func (receiptModules) Module(context.Context, frame.UnitBinding) (browser.Artifact, error) {
	return browser.Artifact{}, frame.ErrUnavailable
}

type receiptBridge struct {
	store       *sqlite.Store
	bridge      *browser.Bridge
	credentials browser.Credentials
	point       frame.ContractRef
}

func openReceiptBridge(t *testing.T) receiptBridge {
	t.Helper()
	admission := frame.NewAdmission()
	store, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "receipt.db"), Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	host, err := frame.NewHost(context.Background(), frame.HostConfig{Admission: admission, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if result, err := host.Close(context.Background()); err != nil || !result.Released {
			t.Errorf("Host cleanup: %+v, %v", result, err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	point, err := frame.NewPoint[struct{}, browser.Object]("test.receipt", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: host, Store: store, Source: receiptModules{}, Points: []frame.ContractRef{point.Ref()}, MaxPending: 1})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := bridge.OpenRealm(t.Context(), "realm", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return receiptBridge{store, bridge, credentials, point.Ref()}
}

func TestSnapshotClosesItsOriginalIdentityAfterReservationCommitFailure(t *testing.T) {
	for _, test := range []struct {
		name            string
		committed       bool
		blockSettlement bool
	}{
		{name: "rollback"},
		{name: "committed", committed: true},
		{name: "unknown settlement", committed: true, blockSettlement: true},
	} {
		for _, closeFirst := range []bool{false, true} {
			name := test.name + "/capture"
			if closeFirst {
				name = test.name + "/close"
			}
			t.Run(name, func(t *testing.T) {
				e := openReceiptBridge(t)
				lost := errors.New("receipt commit acknowledgement lost")
				unavailable := errors.New("receipt settlement unavailable")
				var armed, interrupted, blocked atomic.Bool
				before := func(statement string) error {
					if blocked.Load() && statement == "BEGIN IMMEDIATE" {
						return unavailable
					}
					if !test.committed && statement == "COMMIT" && armed.Load() && interrupted.CompareAndSwap(false, true) {
						return lost
					}
					return nil
				}
				after := func(statement string) error {
					if strings.HasPrefix(statement, "INSERT INTO frame_realm_receipts") {
						armed.Store(true)
					}
					if test.committed && statement == "COMMIT" && armed.Load() && interrupted.CompareAndSwap(false, true) {
						blocked.Store(test.blockSettlement)
						return lost
					}
					return nil
				}
				sqlite.InstallRealmReceiptFault(t, e.store, before, after)
				request := browser.SnapshotRequest{OperationID: "original", Points: []string{e.point.Name}}
				var err error
				if closeFirst {
					err = e.bridge.CloseSnapshot(t.Context(), e.credentials.Token, request.OperationID)
				} else {
					_, err = e.bridge.Snapshot(t.Context(), e.credentials.Token, request)
				}
				if !errors.Is(err, lost) {
					t.Fatalf("original failure was lost: %v", err)
				}
				if test.blockSettlement {
					if err := e.bridge.CloseSnapshot(t.Context(), e.credentials.Token, request.OperationID); !errors.Is(err, unavailable) {
						t.Fatalf("unknown reservation was abandoned: %v", err)
					}
					blocked.Store(false)
				}
				if err := e.bridge.CloseSnapshot(t.Context(), e.credentials.Token, request.OperationID); err != nil {
					t.Fatalf("original snapshot could not close: %v", err)
				}
				receipt, err := e.store.RealmReceipt(t.Context(), request.OperationID)
				if err != nil {
					t.Fatalf("closure has no original receipt: %v", err)
				}
				expected := frame.ErrClosed
				if test.committed && !closeFirst {
					expected = sqlite.ErrIndeterminate
				}
				for range 2 {
					captured, err := e.bridge.Snapshot(t.Context(), e.credentials.Token, request)
					if !errors.Is(err, expected) || captured.ID != "" {
						t.Fatalf("closed original operation recaptured: %+v, %v", captured, err)
					}
					if err := e.bridge.CloseSnapshot(t.Context(), e.credentials.Token, request.OperationID); err != nil {
						t.Fatal(err)
					}
				}
				if repeated, err := e.store.RealmReceipt(t.Context(), request.OperationID); err != nil || !bytes.Equal(repeated, receipt) {
					t.Fatalf("replay rewrote the original receipt: %q, %v", repeated, err)
				}
				fresh := browser.SnapshotRequest{OperationID: "fresh", Points: request.Points}
				if _, err := e.bridge.Snapshot(t.Context(), e.credentials.Token, fresh); err != nil {
					t.Fatalf("settled original operation retained live capacity: %v", err)
				}
				if err := e.bridge.CloseSnapshot(t.Context(), e.credentials.Token, fresh.OperationID); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type snapshotWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *snapshotWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestConcurrentSnapshotCloseClaimsARolledBackReservation(t *testing.T) {
	e := openReceiptBridge(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lost := errors.New("receipt commit was refused")
	committing, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	var armed, interrupted atomic.Bool
	sqlite.InstallRealmReceiptFault(t, e.store, func(statement string) error {
		if statement == "COMMIT" && armed.Load() && interrupted.CompareAndSwap(false, true) {
			close(committing)
			<-resume
			return lost
		}
		return nil
	}, func(statement string) error {
		if strings.HasPrefix(statement, "INSERT INTO frame_realm_receipts") {
			armed.Store(true)
		}
		return nil
	})
	request := browser.SnapshotRequest{OperationID: "original", Points: []string{e.point.Name}}
	captured := make(chan error, 1)
	go func() {
		_, err := e.bridge.Snapshot(ctx, e.credentials.Token, request)
		captured <- err
	}()
	select {
	case <-committing:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closing := &snapshotWaitContext{Context: ctx, entered: make(chan struct{})}
	closed := make(chan error, 1)
	go func() { closed <- e.bridge.CloseSnapshot(closing, e.credentials.Token, request.OperationID) }()
	select {
	case <-closing.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	release.Do(func() { close(resume) })
	if err := <-captured; !errors.Is(err, lost) {
		t.Fatalf("capture lost its rollback: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("joined close could not claim its original identity: %v", err)
	}
	if snapshot, err := e.bridge.Snapshot(ctx, e.credentials.Token, request); !errors.Is(err, frame.ErrClosed) || snapshot.ID != "" {
		t.Fatalf("acknowledged close allowed recapture: %+v, %v", snapshot, err)
	}
}
