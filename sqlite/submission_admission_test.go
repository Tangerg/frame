package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestJointSubmissionRequiresTheOriginalLiveAdmission(t *testing.T) {
	store, owner, admission := testStore(t)
	request := []byte("joint-write")
	if _, err := owner.PrepareUnder(t.Context(), request); err == nil {
		t.Fatal("unadmitted caller prepared a submission")
	}
	foreign := frame.NewAdmission()
	if err := foreign.Admit(t.Context(), func(ctx context.Context) error {
		_, err := owner.PrepareUnder(ctx, request)
		return err
	}); err == nil {
		t.Fatal("foreign authority prepared a submission")
	}
	var escaped context.Context
	var id SubmissionID
	err := admission.Admit(t.Context(), func(ctx context.Context) error {
		escaped = ctx
		var err error
		id, err = owner.PrepareUnder(ctx, request)
		if err != nil {
			return err
		}
		settlement, err := owner.CommitUnder(ctx, id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
			_, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES('joint','converter',?,1)`, value.Hash([]byte("release")))
			return []byte("joint"), err
		}, nil)
		if err == nil && (settlement.State != frame.CommitSucceeded || !settlement.Synchronized) {
			t.Fatalf("joint settlement = %+v", settlement)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.PrepareUnder(escaped, []byte("late")); err == nil {
		t.Fatal("expired admission context created a submission")
	}
	if _, err := owner.CommitUnder(escaped, id, request, func(context.Context, Executor) ([]byte, error) {
		t.Fatal("expired admission ran a mutation")
		return nil, nil
	}, nil); err == nil {
		t.Fatal("expired context reentered the original transaction")
	}
	var count int
	if err := store.Read(t.Context(), func(tx Reader) error {
		return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM documents WHERE id='joint'`).Scan(&count)
	}); err != nil || count != 1 {
		t.Fatalf("joint write count = %d, %v", count, err)
	}
}

func TestSubmissionCapacityRetainsUnsettledPrefixAndReclaimsOnlyConfirmedEvidence(t *testing.T) {
	admission := frame.NewAdmission()
	config := testConfig(filepath.Join(t.TempDir(), "bounded.db"), admission)
	config.MaxSubmissionsPerOwner = 2
	store, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, err := store.Owner("documents")
	if err != nil {
		t.Fatal(err)
	}
	first, err := owner.Prepare(t.Context(), []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := owner.Prepare(t.Context(), []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Prepare(t.Context(), []byte("third")); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("unsettled capacity = %v", err)
	}
	if err := owner.CompactSettled(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Prepare(t.Context(), []byte("third")); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("compaction guessed an unsettled outcome: %v", err)
	}
	if _, err := owner.Commit(t.Context(), first, []byte("first"), func(context.Context, Executor) ([]byte, error) {
		return []byte("original evidence"), nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := owner.CompactSettled(t.Context()); err != nil {
		t.Fatal(err)
	}
	third, err := owner.Prepare(t.Context(), []byte("third"))
	if err != nil || third.Sequence != second.Sequence+1 {
		t.Fatalf("reclaimed capacity = %+v, %v", third, err)
	}
	if _, err := owner.Settlement(t.Context(), first, []byte("first")); !errors.Is(err, ErrEvidenceExpired) {
		t.Fatalf("compacted sequence was reusable: %v", err)
	}
	if _, err := owner.Commit(t.Context(), first, []byte("first"), func(context.Context, Executor) ([]byte, error) {
		t.Fatal("expired original intent was executed again")
		return nil, nil
	}, nil); !errors.Is(err, ErrEvidenceExpired) {
		t.Fatalf("expired commit = %v", err)
	}
}

func TestOnlyTheMatchedRequestCanSettleOrSynchronize(t *testing.T) {
	_, owner, admission := testStore(t)
	request := []byte("original")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	checkedReplay := func() {
		t.Helper()
		err := admission.Admit(t.Context(), func(admitted context.Context) error {
			ctx, cancel := context.WithCancel(admitted)
			cancel()
			settlement, err := owner.CommitUnder(ctx, id, []byte("different"),
				func(context.Context, Executor) ([]byte, error) { t.Fatal("wrong mutation ran"); return nil, nil },
				func(Settlement) error { t.Fatal("wrong request synchronized an original result"); return nil })
			if settlement.Evidence != nil || !settlement.Digest.IsZero() || settlement.State != "" {
				t.Fatalf("wrong request observed original data: %+v", settlement)
			}
			return err
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled unauthenticated request: %v", err)
		}
	}
	checkedReplay()
	applied := false
	state, err := owner.Commit(t.Context(), id, request, func(context.Context, Executor) ([]byte, error) { applied = true; return []byte("evidence"), nil }, nil)
	if err != nil || !applied || state.State != frame.CommitSucceeded {
		t.Fatalf("original submission did not execute: %+v %v", state, err)
	}
	checkedReplay()
	state, err = owner.Settlement(t.Context(), id, []byte("different"))
	if !errors.Is(err, ErrConflict) || state.Evidence != nil || !state.Digest.IsZero() {
		t.Fatalf("read conflict exposed original data: %+v %v", state, err)
	}
}

func TestPostcommitPanicFencesAdmissionUntilTheOwnerRestores(t *testing.T) {
	_, owner, a := testStore(t)
	req := []byte("original")
	id, err := owner.Prepare(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("projection panicked")
	func() {
		defer func() {
			if got := recover(); got != failure {
				t.Fatalf("original callback panic changed: %v", got)
			}
		}()
		_, _ = owner.Commit(t.Context(), id, req, func(context.Context, Executor) ([]byte, error) { return []byte("original"), nil }, func(Settlement) error { panic(failure) })
	}()
	pending, err := owner.Settlement(t.Context(), id, req)
	if err != nil || pending.State != frame.CommitSucceeded || pending.Synchronized || string(pending.Evidence) != "original" {
		t.Fatalf("lost the real commit: %+v, %v", pending, err)
	}
	if _, err := owner.Prepare(t.Context(), []byte("next")); !errors.Is(err, frame.ErrRecoveryRequired) {
		t.Fatalf("new work crossed the unsynchronized commit: %v", err)
	}
	if err := owner.Confirm(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := a.Recover(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Prepare(t.Context(), []byte("next")); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedInstallationReclaimsItsPrivateSubmissionCapacity(t *testing.T) {
	a := frame.NewAdmission()
	cfg := testConfig(filepath.Join(t.TempDir(), "state"), a)
	cfg.MaxSubmissionsPerOwner = 1
	reject := errors.New("index unavailable")
	cfg.Owners[0].ProjectInstallation = func(context.Context, Executor, frame.InstallationChange) error { return reject }
	store, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h, err := frame.NewHost(t.Context(), frame.HostConfig{Admission: a, Installations: store})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())
	p, err := frame.Define(frame.Definition{Revision: value.Hash(nil)})
	if err != nil {
		t.Fatal(err)
	}
	install := frame.InstallationConfig{ID: "example", Plugin: p}
	if _, err := h.Install(t.Context(), install); !errors.Is(err, reject) {
		t.Fatalf("first rejection: %v", err)
	}
	reject = nil
	if _, err := h.Install(t.Context(), install); err != nil {
		t.Fatalf("settled rejection still occupies the private receipt budget: %v", err)
	}
}

func TestInterruptedInstallationProjectionReleasesReceiptBeforeHostRecovery(t *testing.T) {
	for _, interruption := range []string{"panic", "goexit"} {
		t.Run(interruption, func(t *testing.T) {
			admission := frame.NewAdmission()
			config := testConfig(filepath.Join(t.TempDir(), "private-receipts.db"), admission)
			config.MaxSubmissionsPerOwner = 1
			cause := errors.New("installation projection panicked")
			interrupts := true
			config.Owners[0].ProjectInstallation = func(context.Context, Executor, frame.InstallationChange) error {
				if interrupts {
					if interruption == "goexit" {
						runtime.Goexit()
					}
					panic(cause)
				}
				return nil
			}
			store, err := Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			host, err := frame.NewHost(t.Context(), frame.HostConfig{Admission: admission, Installations: store})
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close(context.Background())
			plugin, err := frame.Define(frame.Definition{Revision: value.Hash(nil)})
			if err != nil {
				t.Fatal(err)
			}
			installation := frame.InstallationConfig{ID: "example", Plugin: plugin}
			for range 3 {
				finished := make(chan any, 1)
				returned := false
				go func() {
					defer func() { finished <- recover() }()
					_, _ = host.Install(t.Context(), installation)
					returned = true
				}()
				recovered := <-finished
				if returned {
					t.Fatal("interrupted installation returned normally")
				}
				if interruption == "panic" {
					failure, ok := recovered.(error)
					if !ok || !errors.Is(failure, cause) {
						t.Fatalf("projection panic lost its original cause: %v", recovered)
					}
				} else if recovered != nil {
					t.Fatalf("Goexit acquired an unrelated panic: %v", recovered)
				}
				records, err := store.Installations(t.Context())
				if err != nil || len(records) != 0 {
					t.Fatalf("rolled-back projection retained installation facts: records=%v error=%v", records, err)
				}
				var receipts int
				if err := store.Read(t.Context(), func(reader Reader) error {
					return reader.QueryRowContext(t.Context(), `SELECT count(*) FROM frame_submissions WHERE owner_id=?`, store.control.id).Scan(&receipts)
				}); err != nil || receipts != 0 {
					t.Fatalf("settled interruption retained its private receipt before recovery: count=%d error=%v", receipts, err)
				}
				if !errors.Is(admission.RecoveryError(), frame.ErrRecoveryRequired) {
					t.Fatalf("interrupted Host boundary remained open: %v", admission.RecoveryError())
				}
				// The Host did not observe SQLite's rollback acknowledgement. Restore the
				// original store's facts before allowing another installation attempt.
				records, err = store.RecoverInstallations(t.Context())
				if err != nil || len(records) != 0 {
					t.Fatalf("recovery changed the original rollback: records=%v error=%v", records, err)
				}
				if err := host.Restore(t.Context(), records, nil); err != nil {
					t.Fatal(err)
				}
				if err := store.ConfirmRestoredInstallations(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := admission.Recover(t.Context(), func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			interrupts = false
			if _, err := host.Install(t.Context(), installation); err != nil {
				t.Fatalf("settled interruption still occupies the private receipt budget: %v", err)
			}
		})
	}
}

func TestApplicationCapabilitiesExpireWithTheirOwnerCallback(t *testing.T) {
	store, owner, _ := testStore(t)
	var issuedRead Reader
	if err := store.Read(t.Context(), func(reader Reader) error {
		issuedRead = reader
		if _, writable := reader.(Executor); writable {
			t.Fatal("read projection exposes write authority")
		}
		var id string
		if err := reader.QueryRowContext(t.Context(), `INSERT INTO documents VALUES('denied','p','r',1) RETURNING id`).Scan(&id); err == nil {
			t.Fatal("query-only projection originated a business write")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := issuedRead.QueryContext(t.Context(), `SELECT * FROM documents`); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("read capability survived its callback: %v", err)
	}
	var issuedWrite Executor
	rolledBack := errors.New("rollback")
	err := owner.Update(t.Context(), func(ctx context.Context, tx Executor) error {
		issuedWrite = tx
		if _, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES('original','p','r',1)`); err != nil {
			return err
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatal(err)
	}
	if _, err := issuedWrite.ExecContext(t.Context(), `DELETE FROM documents`); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("write capability survived its callback: %v", err)
	}
	var count int
	if err := store.Read(t.Context(), func(reader Reader) error {
		return reader.QueryRowContext(t.Context(), `SELECT count(*) FROM documents`).Scan(&count)
	}); err != nil || count != 0 {
		t.Fatalf("write escaped its original transaction: count=%d error=%v", count, err)
	}
}
