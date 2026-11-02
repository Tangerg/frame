package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
	modernc "modernc.org/sqlite"
)

// The Store keeps its original authority and gate. Only the database/sql
// connection boundary is decorated; every successful SQL statement still runs
// against the real SQLite file, including commits whose acknowledgement is lost.
type transactionFault struct {
	opened      func()
	beforeExec  func(string) error
	afterExec   func(string) error
	beforeClose func() error
	afterClose  func()
}

type faultConnector struct {
	driver *modernc.Driver
	dsn    string
	fault  transactionFault
}

func (c *faultConnector) Connect(context.Context) (driver.Conn, error) {
	connection, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	if c.fault.opened != nil {
		c.fault.opened()
	}
	return &faultConnection{Conn: connection, fault: c.fault}, nil
}

func (c *faultConnector) Driver() driver.Driver { return c.driver }

type faultConnection struct {
	driver.Conn
	fault  transactionFault
	closed bool
}

func (c *faultConnection) ExecContext(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Result, error) {
	if c.fault.beforeExec != nil {
		if err := c.fault.beforeExec(query); err != nil {
			return nil, err
		}
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, arguments)
	if err == nil && c.fault.afterExec != nil {
		err = c.fault.afterExec(query)
	}
	return result, err
}

func (c *faultConnection) QueryContext(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, arguments)
}

func (c *faultConnection) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *faultConnection) Close() error {
	if c.closed {
		return nil
	}
	if c.fault.beforeClose != nil {
		if err := c.fault.beforeClose(); err != nil {
			return err
		}
	}
	if err := c.Conn.Close(); err != nil {
		return err
	}
	c.closed = true
	if c.fault.afterClose != nil {
		c.fault.afterClose()
	}
	return nil
}

func installTransactionFault(t *testing.T, store *Store, fault transactionFault) string {
	t.Helper()
	var sequence int
	var name, path string
	if err := store.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	store.db = sql.OpenDB(&faultConnector{driver: &modernc.Driver{}, dsn: storeDataSource(path), fault: fault})
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)
	if err := store.db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return path
}

func insertFaultDocument(ctx context.Context, tx Executor, id string) ([]byte, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES(?,'converter',?,1)`, id, framevalue.Hash([]byte("original-release")))
	return []byte("document:" + id), err
}

func assertFaultDocumentCount(t *testing.T, store *Store, want int) {
	t.Helper()
	var count int
	if err := store.Read(t.Context(), func(tx Reader) error {
		return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM documents`).Scan(&count)
	}); err != nil || count != want {
		t.Fatalf("business rows=%d, want %d; error=%v", count, want, err)
	}
}

func TestCommitResponseLossSettlesOriginalDurableConclusion(t *testing.T) {
	store, owner, admission := testStore(t)
	request := []byte("commit-response-loss")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var lost atomic.Bool
	var disposed atomic.Int64
	installTransactionFault(t, store, transactionFault{
		afterExec: func(query string) error {
			if query == "COMMIT" && lost.CompareAndSwap(false, true) {
				return errors.New("original commit acknowledgement lost")
			}
			return nil
		},
		afterClose: func() { disposed.Add(1) },
	})
	mutations := 0
	settlement, err := owner.Commit(t.Context(), id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
		mutations++
		return insertFaultDocument(ctx, tx, "committed")
	}, nil)
	if err != nil || settlement.State != Committed || !settlement.Synchronized || !lost.Load() || disposed.Load() != 1 {
		t.Fatalf("original commit reconciliation: %+v error=%v lost=%t disposed=%d", settlement, err, lost.Load(), disposed.Load())
	}
	if err := admission.Admit(t.Context(), func(ctx context.Context) error { return admission.CheckUnused(ctx, "converter") }); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("reopened admission missed the original committed dependency: %v", err)
	}
	_, err = owner.Commit(t.Context(), id, request, func(context.Context, Executor) ([]byte, error) {
		mutations++
		return nil, nil
	}, nil)
	if err != nil || mutations != 1 {
		t.Fatalf("original commit replayed: mutations=%d error=%v", mutations, err)
	}
	assertFaultDocumentCount(t, store, 1)
}

func TestInFlightCommitRetainsAdmissionThroughCallerCancellation(t *testing.T) {
	for _, phase := range []string{"before-commit", "after-commit"} {
		t.Run(phase, func(t *testing.T) {
			store, owner, admission := testStore(t)
			request := []byte(phase)
			id, err := owner.Prepare(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			reached, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var injected atomic.Bool
			block := func(query string) error {
				if query == "COMMIT" && injected.CompareAndSwap(false, true) {
					close(reached)
					<-release
				}
				return nil
			}
			fault := transactionFault{beforeExec: block}
			if phase == "after-commit" {
				fault = transactionFault{afterExec: block}
			}
			path := installTransactionFault(t, store, fault)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type result struct {
				settlement Settlement
				err        error
			}
			completed := make(chan result, 1)
			go func() {
				settlement, err := owner.Commit(ctx, id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
					return insertFaultDocument(ctx, tx, phase)
				}, nil)
				completed <- result{settlement, err}
			}()
			select {
			case <-reached:
			case <-t.Context().Done():
				t.Fatal("original COMMIT was not reached")
			}
			cancel()
			reader, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			var visible int
			err = reader.QueryRowContext(t.Context(), `SELECT count(*) FROM documents`).Scan(&visible)
			if closeErr := reader.Close(); err != nil || closeErr != nil {
				t.Fatal(errors.Join(err, closeErr))
			}
			want := 0
			if phase == "after-commit" {
				want = 1
			}
			if visible != want {
				t.Fatalf("fault did not bracket the real SQLite COMMIT: visible=%d want=%d", visible, want)
			}
			wait, stop := context.WithTimeout(t.Context(), 25*time.Millisecond)
			var entered bool
			err = admission.Under(wait, func() error { entered = true; return nil })
			stop()
			if entered || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("admission reopened around in-flight COMMIT: entered=%t error=%v", entered, err)
			}
			select {
			case value := <-completed:
				t.Fatalf("caller cancellation abandoned original COMMIT: %+v", value)
			default:
			}
			unblock()
			value := <-completed
			if value.err != nil || value.settlement.State != Committed || !value.settlement.Synchronized {
				t.Fatalf("known commit lost to cancellation: %+v error=%v", value.settlement, value.err)
			}
			if err := admission.Admit(t.Context(), func(ctx context.Context) error { return admission.CheckUnused(ctx, "converter") }); !errors.Is(err, frame.ErrInUse) {
				t.Fatalf("post-commit change missed the new dependency: %v", err)
			}
		})
	}
}

func TestFailedRollbackJoinsOriginalConnectionDisposal(t *testing.T) {
	store, owner, admission := testStore(t)
	request := []byte("rollback-disposal")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var failed atomic.Bool
	var disposed atomic.Int64
	installTransactionFault(t, store, transactionFault{
		beforeExec: func(query string) error {
			if query == "ROLLBACK" && failed.CompareAndSwap(false, true) {
				close(reached)
				<-release
				return errors.New("rollback completion unavailable")
			}
			return nil
		},
		afterClose: func() { disposed.Add(1) },
	})
	mutationFailure := errors.New("business mutation rejected")
	type result struct {
		settlement Settlement
		err        error
	}
	completed := make(chan result, 1)
	go func() {
		settlement, err := owner.Commit(t.Context(), id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
			_, err := insertFaultDocument(ctx, tx, "must-rollback")
			return nil, errors.Join(err, mutationFailure)
		}, nil)
		completed <- result{settlement, err}
	}()
	<-reached
	wait, stop := context.WithTimeout(t.Context(), 25*time.Millisecond)
	err = admission.Under(wait, func() error { t.Error("admission reopened before rollback/disposal"); return nil })
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rollback wait did not retain admission: %v", err)
	}
	unblock()
	value := <-completed
	if value.settlement.State != NotCommitted || !value.settlement.Synchronized || !errors.Is(value.err, mutationFailure) || disposed.Load() != 1 {
		t.Fatalf("original disposal did not settle rollback: %+v error=%v disposed=%d", value.settlement, value.err, disposed.Load())
	}
	if err := admission.Under(t.Context(), func() error { return nil }); err != nil {
		t.Fatalf("confirmed disposal left an unresolved admission: %v", err)
	}
	assertFaultDocumentCount(t, store, 0)
	_, err = owner.Commit(t.Context(), id, request, func(context.Context, Executor) ([]byte, error) {
		t.Fatal("the original rejected mutation replayed")
		return nil, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepareResponseLossPreservesOneIssuedIntent(t *testing.T) {
	store, owner, _ := testStore(t)
	var lost atomic.Bool
	installTransactionFault(t, store, transactionFault{afterExec: func(query string) error {
		if query == "COMMIT" && lost.CompareAndSwap(false, true) {
			return errors.New("intent allocation acknowledgement lost")
		}
		return nil
	}})
	request := []byte("prepared-not-dispatched")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil || !lost.Load() || id.Sequence != 1 {
		t.Fatalf("original allocated intent: %+v error=%v injected=%t", id, err, lost.Load())
	}
	pending, err := owner.Pending(t.Context())
	if err != nil || len(pending) != 1 || pending[0].ID != id || pending[0].State != Indeterminate {
		t.Fatalf("issued intent lost or duplicated: %+v error=%v", pending, err)
	}
	assertFaultDocumentCount(t, store, 0)
	next, err := owner.Prepare(t.Context(), []byte("next"))
	if err != nil || next.Sequence != id.Sequence+1 {
		t.Fatalf("allocation acknowledgement created a sequence hole: %+v error=%v", next, err)
	}
}

func TestUnconfirmedCloseAdvancesOnlyTheOriginalConnection(t *testing.T) {
	store, owner, admission := testStore(t)
	request := []byte("original-close")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var opens, closeAttempts, released atomic.Int64
	var completionFailed, disposalAllowed atomic.Bool
	path := installTransactionFault(t, store, transactionFault{
		opened: func() { opens.Add(1) },
		beforeExec: func(query string) error {
			if query == "COMMIT" && completionFailed.CompareAndSwap(false, true) || query == "ROLLBACK" {
				return errors.New("transaction completion unconfirmed")
			}
			return nil
		},
		beforeClose: func() error {
			closeAttempts.Add(1)
			if !disposalAllowed.Load() {
				return errors.New("original connection remains open")
			}
			return nil
		},
		afterClose: func() { released.Add(1) },
	})
	defer disposalAllowed.Store(true)
	settlement, err := owner.Commit(t.Context(), id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
		return insertFaultDocument(ctx, tx, "discarded")
	}, nil)
	if !errors.Is(err, ErrIndeterminate) || settlement.State != Indeterminate || len(settlement.Evidence) != 0 || settlement.Synchronized {
		t.Fatalf("unconfirmed commit exposed a speculative conclusion: %+v error=%v", settlement, err)
	}
	if err := store.Close(); !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("unconfirmed original close: %v", err)
	}
	if lock, err := acquireAuthority(path); err == nil {
		_ = releaseAuthority(lock)
		t.Fatal("an unconfirmed close relinquished the original authority")
	}
	if opens.Load() != 1 || released.Load() != 0 || closeAttempts.Load() != 2 {
		t.Fatalf("close replaced or prematurely released its original connection: opens=%d attempts=%d released=%d", opens.Load(), closeAttempts.Load(), released.Load())
	}
	disposalAllowed.Store(true)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 || released.Load() != 1 || closeAttempts.Load() != 3 {
		t.Fatalf("terminal close reran cleanup or replaced its connection: opens=%d attempts=%d released=%d", opens.Load(), closeAttempts.Load(), released.Load())
	}
	if admission.RecoveryError() == nil {
		t.Fatal("connection release cleared the original recovery requirement")
	}
	lock, err := acquireAuthority(path)
	if err != nil {
		t.Fatalf("confirmed original release retained the authority: %v", err)
	}
	if err := releaseAuthority(lock); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), testConfig(path, frame.NewAdmission()))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertFaultDocumentCount(t, reopened, 0)
	original, _ := reopened.Owner("documents")
	if err := original.RecoverPrepared(t.Context()); err != nil {
		t.Fatal(err)
	}
	settlement, err = original.Settlement(t.Context(), id, request)
	if err != nil || settlement.State != NotCommitted {
		t.Fatalf("original release did not settle its exact uncommitted intent: %+v error=%v", settlement, err)
	}
}

func TestStoreCloseJoinsAnInFlightOriginalCommit(t *testing.T) {
	store, owner, _ := testStore(t)
	request := []byte("close-with-commit")
	id, err := owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var blocked atomic.Bool
	path := installTransactionFault(t, store, transactionFault{beforeExec: func(query string) error {
		if query == "COMMIT" && blocked.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
		return nil
	}})
	type commitResult struct {
		settlement Settlement
		err        error
	}
	committed := make(chan commitResult, 1)
	go func() {
		settlement, err := owner.Commit(t.Context(), id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
			return insertFaultDocument(ctx, tx, "closing")
		}, nil)
		committed <- commitResult{settlement, err}
	}()
	<-reached
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	wait, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	select {
	case err := <-closed:
		t.Fatalf("Store.Close completed while original COMMIT could still run: %v", err)
	case <-wait.Done():
	}
	if lock, err := acquireAuthority(path); err == nil {
		_ = releaseAuthority(lock)
		t.Fatal("in-flight original COMMIT lost its file authority")
	}
	unblock()
	value := <-committed
	if value.settlement.State != Committed || value.err != nil && !errors.Is(value.err, frame.ErrClosed) {
		t.Fatalf("original commit result during shutdown: %+v error=%v", value.settlement, value.err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), testConfig(path, frame.NewAdmission()))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	original, _ := reopened.Owner("documents")
	settlement, err := original.Settlement(t.Context(), id, request)
	if err != nil || settlement.State != Committed {
		t.Fatalf("shutdown changed the original durable conclusion: %+v error=%v", settlement, err)
	}
	assertFaultDocumentCount(t, reopened, 1)
}

func TestBusinessRowUpdateFencesAnUnacknowledgedCommit(t *testing.T) {
	store, owner, a := testStore(t)
	lost := errors.New("commit reply lost")
	var injected atomic.Bool
	installTransactionFault(t, store, transactionFault{afterExec: func(query string) error {
		if query == "COMMIT" && injected.CompareAndSwap(false, true) {
			return lost
		}
		return nil
	}})
	err := owner.Update(t.Context(), func(ctx context.Context, tx Executor) error {
		_, err := insertFaultDocument(ctx, tx, "original")
		return err
	})
	if !errors.Is(err, lost) || !errors.Is(err, ErrIndeterminate) || !errors.Is(a.RecoveryError(), frame.ErrRecoveryRequired) {
		t.Fatalf("unacknowledged business commit reopened admission: %v %v", err, a.RecoveryError())
	}
	assertFaultDocumentCount(t, store, 1)
}
