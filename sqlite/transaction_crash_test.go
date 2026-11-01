package sqlite

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
)

type transactionCut struct {
	Phase    string          `json:"phase"`
	IDs      []SubmissionID  `json:"ids,omitempty"`
	State    SettlementState `json:"state,omitempty"`
	Recovery bool            `json:"recovery,omitempty"`
	CloseErr string          `json:"closeError,omitempty"`
}

func startTransactionCut(t *testing.T, phase string) (string, transactionCut, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cut.db")
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSQLiteTransactionCutChild$")
	command.Env = append(os.Environ(), "FRAME_TRANSACTION_CUT="+phase, "FRAME_TRANSACTION_PATH="+path)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var stopped sync.Once
	kill := func() {
		stopped.Do(func() {
			if err := command.Process.Kill(); err != nil {
				t.Errorf("kill original SQLite owner: %v", err)
			}
			if err := command.Wait(); err == nil {
				t.Error("SQLite owner did not terminate at its injected cut")
			}
			_ = stdin.Close()
		})
	}
	t.Cleanup(kill)
	var cut transactionCut
	if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&cut); err != nil {
		kill()
		t.Fatalf("SQLite cut was not reached: %v; stderr=%s", err, stderr.String())
	}
	if cut.Phase != phase {
		t.Fatalf("wrong SQLite cut: %+v", cut)
	}
	return path, cut, kill
}

func TestUnconfirmedDisposalRetainsAuthorityUntilOriginalProcessExit(t *testing.T) {
	path, cut, kill := startTransactionCut(t, "unconfirmed-disposal")
	if cut.State != Indeterminate || !cut.Recovery || cut.CloseErr == "" || len(cut.IDs) != 1 {
		t.Errorf("unconfirmed original transaction was reported as settled: %+v", cut)
	}
	// The old connection can still commit. Its Store must retain the real file
	// authority even when closing the database/sql wrapper returns an error.
	if lock, err := acquireAuthority(path); err == nil {
		_ = releaseAuthority(lock)
		t.Error("unconfirmed connection disposal released the original authority")
	}
	kill()
	admission := frame.NewAdmission()
	store, err := Open(t.Context(), testConfig(path, admission))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.Owner("documents")
	if err != nil {
		t.Fatal(err)
	}
	if admission.RecoveryError() == nil {
		t.Fatal("reopening an unfinished original intent did not require recovery")
	}
	if err := owner.RecoverPrepared(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(cut.IDs) != 1 {
		t.Fatal("child did not report its original issued intent")
	}
	settlement, err := owner.Settlement(t.Context(), cut.IDs[0], []byte("unconfirmed-disposal"))
	if err != nil || settlement.State != NotCommitted {
		t.Fatalf("original connection committed after process exit: %+v error=%v", settlement, err)
	}
	assertFaultDocumentCount(t, store, 0)
	if err := owner.Confirm(t.Context(), cut.IDs[0]); err != nil {
		t.Fatal(err)
	}
	if admission.RecoveryError() == nil {
		t.Fatal("one settlement cleared the host recovery barrier")
	}
	if err := admission.Recover(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, err = owner.Commit(t.Context(), cut.IDs[0], []byte("unconfirmed-disposal"), func(context.Context, Executor) ([]byte, error) {
		t.Fatal("the unresolved original transaction replayed after recovery")
		return nil, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepareAcknowledgementCrashRecoversAllocatedOriginalIntent(t *testing.T) {
	path, _, kill := startTransactionCut(t, "prepare-committed")
	kill()
	admission := frame.NewAdmission()
	store, err := Open(t.Context(), testConfig(path, admission))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, _ := store.Owner("documents")
	pending, err := owner.Pending(t.Context())
	if err != nil || len(pending) != 1 || pending[0].ID.Sequence != 1 || pending[0].State != Indeterminate {
		t.Fatalf("allocation response loss left a missing or duplicate intent: %+v error=%v", pending, err)
	}
	if admission.RecoveryError() == nil {
		t.Fatal("undelivered Prepared intent did not fence normal admission")
	}
	assertFaultDocumentCount(t, store, 0)
	if err := owner.RecoverPrepared(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := pending[0].ID
	settlement, err := owner.Settlement(t.Context(), id, []byte("prepare-committed"))
	if err != nil || settlement.State != NotCommitted {
		t.Fatalf("undispatched allocation was not settled from its original evidence: %+v error=%v", settlement, err)
	}
	if err := owner.Confirm(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := admission.Recover(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, err = owner.Commit(t.Context(), id, []byte("prepare-committed"), func(context.Context, Executor) ([]byte, error) {
		t.Fatal("an allocation with a settled original outcome dispatched on replay")
		return nil, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := owner.Prepare(t.Context(), []byte("after-recovery"))
	if err != nil || next.Sequence != id.Sequence+1 {
		t.Fatalf("allocated sequence was lost or reused: %+v error=%v", next, err)
	}
}

func TestCompactionCrashKeepsWatermarkAndEvidenceAtomic(t *testing.T) {
	for _, phase := range []string{"compact-deleted", "compact-watermark", "compact-committed"} {
		t.Run(phase, func(t *testing.T) {
			path, cut, kill := startTransactionCut(t, phase)
			kill()
			store, err := Open(t.Context(), testConfig(path, frame.NewAdmission()))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			owner, _ := store.Owner("documents")
			if len(cut.IDs) != 2 {
				t.Fatalf("compaction did not retain its original issued range: %+v", cut)
			}
			var watermark uint64
			if err := store.Read(t.Context(), func(tx Executor) error {
				return tx.QueryRowContext(t.Context(), `SELECT compacted_through FROM frame_owners WHERE id='documents'`).Scan(&watermark)
			}); err != nil {
				t.Fatal(err)
			}
			wantWatermark := uint64(0)
			if phase == "compact-committed" {
				wantWatermark = cut.IDs[1].Sequence
			}
			if watermark != wantWatermark {
				t.Fatalf("watermark=%d, want original committed value %d", watermark, wantWatermark)
			}
			for index, id := range cut.IDs {
				request := []byte([]string{"first", "second"}[index])
				settlement, err := owner.Settlement(t.Context(), id, request)
				if phase == "compact-committed" {
					if !errors.Is(err, ErrEvidenceExpired) {
						t.Fatalf("committed watermark retained/recreated expired evidence: %+v error=%v", settlement, err)
					}
				} else if err != nil || settlement.State != Committed || !settlement.Synchronized {
					t.Fatalf("uncommitted compaction lost original evidence: %+v error=%v", settlement, err)
				}
				_, err = owner.Commit(t.Context(), id, request, func(context.Context, Executor) ([]byte, error) {
					t.Fatal("compacted or original terminal operation replayed")
					return nil, nil
				}, nil)
				if phase == "compact-committed" {
					if !errors.Is(err, ErrEvidenceExpired) {
						t.Fatalf("expired replay was not refused: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			assertFaultDocumentCount(t, store, 2)
			next, err := owner.Prepare(t.Context(), []byte("third"))
			if err != nil || next.Sequence != cut.IDs[1].Sequence+1 {
				t.Fatalf("compaction reused an original operation identity: %+v error=%v", next, err)
			}
		})
	}
}

func TestSQLiteTransactionCutChild(t *testing.T) {
	phase := os.Getenv("FRAME_TRANSACTION_CUT")
	if phase == "" {
		return
	}
	admission := frame.NewAdmission()
	store, err := Open(context.Background(), testConfig(os.Getenv("FRAME_TRANSACTION_PATH"), admission))
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := store.Owner("documents")
	cut := transactionCut{Phase: phase}
	pause := func() {
		if err := json.NewEncoder(os.Stdout).Encode(cut); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		t.Fatal("parent did not SIGKILL the original owner at the declared cut")
	}
	if phase == "prepare-committed" {
		var reached atomic.Bool
		installTransactionFault(t, store, transactionFault{afterExec: func(query string) error {
			if query == "COMMIT" && reached.CompareAndSwap(false, true) {
				pause()
			}
			return nil
		}})
		_, err = owner.Prepare(t.Context(), []byte(phase))
		t.Fatalf("Prepare returned before the selected crash cut: %v", err)
	}
	if phase == "unconfirmed-disposal" {
		id, err := owner.Prepare(t.Context(), []byte(phase))
		if err != nil {
			t.Fatal(err)
		}
		cut.IDs = []SubmissionID{id}
		installTransactionFault(t, store, transactionFault{
			beforeExec: func(query string) error {
				if query == "COMMIT" || query == "ROLLBACK" {
					return errors.New("original transaction completion is unavailable")
				}
				return nil
			},
			beforeClose: func() error { return errors.New("original SQLite connection disposal is unconfirmed") },
		})
		settlement, err := owner.Commit(t.Context(), id, []byte(phase), func(ctx context.Context, tx Executor) ([]byte, error) {
			return insertFaultDocument(ctx, tx, "still-uncommitted")
		}, nil)
		if !errors.Is(err, ErrIndeterminate) {
			t.Fatalf("original transaction was not left unresolved: %+v error=%v", settlement, err)
		}
		cut.State = settlement.State
		cut.Recovery = errors.Is(admission.RecoveryError(), frame.ErrRecoveryRequired)
		if err := store.Close(); err != nil {
			cut.CloseErr = err.Error()
		}
		owner, store = nil, nil
		runtime.GC()
		runtime.GC()
		pause()
	}
	if !strings.HasPrefix(phase, "compact-") {
		t.Fatalf("unknown transaction cut %q", phase)
	}
	for _, request := range []string{"first", "second"} {
		id, err := owner.Prepare(t.Context(), []byte(request))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Commit(t.Context(), id, []byte(request), func(ctx context.Context, tx Executor) ([]byte, error) {
			return insertFaultDocument(ctx, tx, request)
		}, nil); err != nil {
			t.Fatal(err)
		}
		cut.IDs = append(cut.IDs, id)
	}
	installTransactionFault(t, store, transactionFault{afterExec: func(query string) error {
		if phase == "compact-deleted" && strings.HasPrefix(query, "DELETE FROM frame_submissions ") ||
			phase == "compact-watermark" && strings.HasPrefix(query, "UPDATE frame_owners SET compacted_through=") ||
			phase == "compact-committed" && query == "COMMIT" {
			pause()
		}
		return nil
	}})
	if err := owner.Compact(t.Context(), cut.IDs[1].Sequence); err != nil {
		t.Fatal(err)
	}
	t.Fatal("compaction returned before the selected crash cut")
}
