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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

var errParentFinal = errors.New("jobs: parent is final")

type childAcceptanceFixture struct {
	store     *Store
	owner     *Owner
	admission *frame.Admission
	host      *frame.Host
	parent    frame.ExportRef
	child     frame.ExportRef
	contract  frame.Contract[func() string]
	calls     atomic.Int64
}

func childAcceptanceConfig(path string, admission *frame.Admission) Config {
	return Config{Path: path, Admission: admission, Owners: []OwnerSchema{{
		ID: "jobs",
		Statements: []string{
			`CREATE TABLE parent_jobs (id TEXT PRIMARY KEY, active INTEGER NOT NULL, binding BLOB NOT NULL)`,
			`CREATE TABLE child_jobs (id TEXT PRIMARY KEY, operation TEXT UNIQUE NOT NULL, parent TEXT NOT NULL REFERENCES parent_jobs(id), active INTEGER NOT NULL, binding BLOB NOT NULL)`,
		},
		References: func(ctx context.Context, tx Executor, installation frame.InstallationID) ([]frame.Dependency, error) {
			rows, err := tx.QueryContext(ctx, `SELECT binding FROM parent_jobs WHERE active=1 UNION ALL SELECT binding FROM child_jobs WHERE active=1`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var retained []frame.Dependency
			for rows.Next() {
				var encoded []byte
				var binding frame.BindingSnapshot
				if err := rows.Scan(&encoded); err != nil {
					return nil, err
				}
				if err := json.Unmarshal(encoded, &binding); err != nil {
					return nil, err
				}
				for _, dependency := range binding.Dependencies() {
					if dependency.InstallationID == installation {
						retained = append(retained, dependency)
					}
				}
			}
			return retained, rows.Err()
		},
	}}}
}

func openChildAcceptance(t *testing.T, path string, initialize bool) *childAcceptanceFixture {
	t.Helper()
	f := &childAcceptanceFixture{admission: frame.NewAdmission()}
	var err error
	f.store, err = Open(t.Context(), childAcceptanceConfig(path, f.admission))
	if err != nil {
		if f.store != nil {
			err = errors.Join(err, f.store.Close())
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	})
	f.owner, err = f.store.Owner("jobs")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.admission.RegisterUseOwner(f.owner); err != nil {
		t.Fatal(err)
	}
	if !initialize {
		if err := f.owner.RecoverPrepared(t.Context()); err != nil {
			t.Fatal(err)
		}
		pending, err := f.owner.Pending(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, conclusion := range pending {
			if err := f.owner.Confirm(t.Context(), conclusion.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.admission.Recover(t.Context(), func() error {
			_, err := f.owner.Held(t.Context(), "child-provider")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.host, err = frame.NewHost(t.Context(), frame.HostConfig{Admission: f.admission, ReadAcceptedBinding: func(ctx context.Context, id string, _ frame.ScopeGrant) (frame.BindingSnapshot, error) {
		var data []byte
		if err := f.store.Read(ctx, func(tx Executor) error {
			return tx.QueryRowContext(ctx, "SELECT binding FROM child_jobs WHERE operation=? AND active=1", id).Scan(&data)
		}); err != nil {
			return frame.BindingSnapshot{}, err
		}
		return frame.ParseBinding(data)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		outcome, err := f.host.Close(ctx)
		if err != nil || !outcome.Released {
			t.Errorf("close consumer Host: %+v, %v", outcome, err)
		}
	})
	f.contract, err = frame.NewContract[func() string]("jobs.run", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.host.Register(f.contract); err != nil {
		t.Fatal(err)
	}
	for _, id := range []frame.InstallationID{"parent-provider", "child-provider"} {
		plugin, err := frame.Define(frame.Definition{Revision: value.Hash([]byte(id)), Units: []frame.Unit{{
			Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "run", Contract: f.contract.Ref()}}},
			Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
				return []frame.ExportValue{frame.Provide("run", f.contract, func() string { f.calls.Add(1); return string(id) })}, nil
			},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.Install(t.Context(), frame.InstallationConfig{ID: id, Plugin: plugin, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		root := frame.ExportRef{InstallationID: id, Release: plugin.Revision(), UnitID: "main", Export: "run", Contract: f.contract.Ref()}
		if id == "parent-provider" {
			f.parent = root
		} else {
			f.child = root
		}
	}
	if initialize {
		request := []byte("open-parent")
		id := f.prepare(t, request)
		plan := f.plan(t, f.parent)
		settlement, err := f.owner.Commit(t.Context(), id, request, func(ctx context.Context, tx Executor) ([]byte, error) {
			binding, err := f.host.ValidatePlanUnder(ctx, plan)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(binding)
			if err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO parent_jobs VALUES('parent',1,?)`, encoded)
			return []byte("parent"), err
		}, nil)
		if err != nil || settlement.State != Committed {
			t.Fatalf("initialize parent: %+v, %v", settlement, err)
		}
	}
	return f
}

func (f *childAcceptanceFixture) prepare(t *testing.T, request []byte) SubmissionID {
	t.Helper()
	id, err := f.owner.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *childAcceptanceFixture) plan(t *testing.T, root frame.ExportRef) *frame.BindingPlan {
	t.Helper()
	plan, err := f.host.Plan(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func (f *childAcceptanceFixture) accept(plan *frame.BindingPlan, operation string) func(context.Context, Executor) ([]byte, error) {
	return func(ctx context.Context, tx Executor) ([]byte, error) {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT active FROM parent_jobs WHERE id='parent'`).Scan(&active); err != nil {
			return nil, err
		}
		if active != 1 {
			return nil, errParentFinal
		}
		binding, err := f.host.ValidatePlanUnder(ctx, plan)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(binding)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO child_jobs VALUES('child',?,'parent',1,?)`, operation, encoded)
		return []byte(operation), err
	}
}

func finishChildParent(ctx context.Context, tx Executor) ([]byte, error) {
	_, err := tx.ExecContext(ctx, `UPDATE parent_jobs SET active=0 WHERE id='parent'`)
	return []byte("parent is final"), err
}

func (f *childAcceptanceFixture) childBinding(t *testing.T) (string, frame.BindingSnapshot) {
	t.Helper()
	var operation string
	var encoded []byte
	if err := f.store.Read(t.Context(), func(tx Executor) error {
		return tx.QueryRowContext(t.Context(), `SELECT operation,binding FROM child_jobs WHERE id='child'`).Scan(&operation, &encoded)
	}); err != nil {
		t.Fatal(err)
	}
	var binding frame.BindingSnapshot
	if err := json.Unmarshal(encoded, &binding); err != nil {
		t.Fatal(err)
	}
	return operation, binding
}

func TestDurableChildAcceptanceAndParentFinalizationShareOneAdmission(t *testing.T) {
	for _, first := range []string{"child", "parent"} {
		t.Run(first+" wins", func(t *testing.T) {
			f := openChildAcceptance(t, filepath.Join(t.TempDir(), "jobs.db"), true)
			childRequest, parentRequest := []byte("child-operation"), []byte("finish-parent")
			childID, parentID := f.prepare(t, childRequest), f.prepare(t, parentRequest)
			childMutation := f.accept(f.plan(t, f.child), string(childRequest))
			entered, release := make(chan struct{}), make(chan struct{})
			resume := sync.OnceFunc(func() { close(release) })
			defer resume()
			block := func(mutation func(context.Context, Executor) ([]byte, error)) func(context.Context, Executor) ([]byte, error) {
				return func(ctx context.Context, tx Executor) ([]byte, error) {
					evidence, err := mutation(ctx, tx)
					close(entered)
					select {
					case <-release:
						return evidence, err
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
			type result struct {
				settlement Settlement
				err        error
			}
			childDone, parentDone := make(chan result, 1), make(chan result, 1)
			commitChild := func(mutation func(context.Context, Executor) ([]byte, error)) {
				settlement, err := f.owner.Commit(t.Context(), childID, childRequest, mutation, nil)
				childDone <- result{settlement, err}
			}
			commitParent := func(mutation func(context.Context, Executor) ([]byte, error)) {
				settlement, err := f.owner.Commit(t.Context(), parentID, parentRequest, mutation, nil)
				parentDone <- result{settlement, err}
			}
			if first == "child" {
				go commitChild(block(childMutation))
			} else {
				go commitParent(block(finishChildParent))
			}
			<-entered
			if first == "child" {
				go commitParent(finishChildParent)
			} else {
				go commitChild(childMutation)
			}
			if f.calls.Load() != 0 {
				t.Fatal("a business effect ran before its durable acceptance")
			}
			resume()
			parentResult, childResult := <-parentDone, <-childDone
			if parentResult.err != nil || parentResult.settlement.State != Committed {
				t.Fatalf("parent finalization: %+v", parentResult)
			}
			if first == "child" {
				if childResult.err != nil || childResult.settlement.State != Committed {
					t.Fatalf("original child acceptance: %+v", childResult)
				}
				operation, binding := f.childBinding(t)
				if operation != string(childRequest) || len(binding.Roots) != 1 || binding.Roots[0] != f.child {
					t.Fatalf("child lost its original operation or binding: %q, %+v", operation, binding)
				}
				if err := f.host.Uninstall(t.Context(), f.child.InstallationID); !errors.Is(err, frame.ErrInUse) {
					t.Fatalf("parent finalization discarded child retention: %v", err)
				}
			} else {
				if !errors.Is(childResult.err, errParentFinal) || childResult.settlement.State != NotCommitted {
					t.Fatalf("child bypassed final parent precondition: %+v", childResult)
				}
				var children int
				if err := f.store.Read(t.Context(), func(tx Executor) error {
					return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM child_jobs`).Scan(&children)
				}); err != nil || children != 0 {
					t.Fatalf("rejected child survived: %d, %v", children, err)
				}
				if err := f.host.Uninstall(t.Context(), f.child.InstallationID); err != nil {
					t.Fatalf("rejected child invented retention: %v", err)
				}
			}
			if err := f.host.Uninstall(t.Context(), f.parent.InstallationID); err != nil {
				t.Fatalf("final parent retained its own provider: %v", err)
			}
		})
	}
}

func TestCommittedChildSurvivesParentUseClosureWithoutAutomaticDispatch(t *testing.T) {
	f := openChildAcceptance(t, filepath.Join(t.TempDir(), "jobs.db"), true)
	caller := frame.NewLifetime(context.Background())
	t.Cleanup(func() { _, _ = caller.Close(context.Background()) })
	scope, err := f.admission.OpenScope(t.Context(), "parent-execution")
	if err != nil {
		t.Fatal(err)
	}
	parentUse, err := f.host.Acquire(t.Context(), f.plan(t, f.parent), frame.UseConfig{Owner: caller, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	request := []byte("child-after-parent-use")
	id := f.prepare(t, request)
	plan := f.plan(t, f.child)
	var lost atomic.Bool
	installTransactionFault(t, f.store, transactionFault{afterExec: func(query string) error {
		if query == "COMMIT" && lost.CompareAndSwap(false, true) {
			parentUse.BeginClose(errors.New("parent execution ended"))
			return errors.New("child commit response was lost")
		}
		return nil
	}})
	settlement, err := f.owner.Commit(t.Context(), id, request, f.accept(plan, string(request)), nil)
	if err != nil || settlement.State != Committed || !lost.Load() {
		t.Fatalf("original accepted child was not settled: %+v, %v", settlement, err)
	}
	if outcome, err := parentUse.Close(t.Context()); err != nil || !outcome.Released {
		t.Fatalf("parent Use did not finish its original close: %+v, %v", outcome, err)
	}
	if childUse, err := f.host.AcquireChild(t.Context(), parentUse, plan); !errors.Is(err, frame.ErrClosed) || childUse != nil {
		t.Fatalf("closed parent dispatched a late child: %v, %v", childUse, err)
	}
	finishRequest := []byte("finish-after-execution")
	if _, err := f.owner.Commit(t.Context(), f.prepare(t, finishRequest), finishRequest, finishChildParent, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.host.Uninstall(t.Context(), f.child.InstallationID); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("parent Use closure erased durable child acceptance: %v", err)
	}
	operation, binding := f.childBinding(t)
	if operation != string(request) || len(binding.Roots) != 1 || binding.Roots[0] != f.child || f.calls.Load() != 0 {
		t.Fatalf("accepted child changed or was automatically dispatched: %q, %+v, calls=%d", operation, binding, f.calls.Load())
	}
	repeated, err := f.owner.Commit(t.Context(), id, request, func(context.Context, Executor) ([]byte, error) {
		t.Fatal("observing accepted child replayed its mutation")
		return nil, nil
	}, nil)
	if err != nil || repeated.State != Committed || string(repeated.Evidence) != string(request) {
		t.Fatalf("child acceptance was rewritten after parent exit: %+v, %v", repeated, err)
	}
}

type childCommitCut struct {
	Phase   string       `json:"phase"`
	ID      SubmissionID `json:"id"`
	Calls   int64        `json:"calls"`
	Unknown bool         `json:"unknown"`
}

func TestDurableChildCrashRecoveryUsesOriginalAcceptanceAndOperation(t *testing.T) {
	for _, phase := range []string{"before-commit", "commit-unconfirmed", "committed-before-response"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.db")
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDurableChildCommitCutProcess$")
			command.Env = append(os.Environ(), "FRAME_CHILD_CUT="+phase, "FRAME_CHILD_PATH="+path)
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
			kill := sync.OnceFunc(func() {
				if err := command.Process.Kill(); err != nil {
					t.Errorf("kill original child admission owner: %v", err)
				}
				if err := command.Wait(); err == nil {
					t.Error("child admission owner did not crash")
				}
				_ = stdin.Close()
			})
			t.Cleanup(kill)
			var cut childCommitCut
			if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&cut); err != nil {
				kill()
				t.Fatalf("child commit cut not reached: %v; stderr=%s", err, stderr.String())
			}
			if cut.Phase != phase || cut.Calls != 0 || phase == "commit-unconfirmed" && !cut.Unknown {
				t.Fatalf("child dispatched or did not reach its original boundary: %+v", cut)
			}
			kill()
			f := openChildAcceptance(t, path, false)
			request := []byte("durable-child:" + phase)
			settlement, err := f.owner.Commit(t.Context(), cut.ID, request, func(context.Context, Executor) ([]byte, error) {
				t.Fatal("recovery replayed the original child submission")
				return nil, nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "committed-before-response" {
				if settlement.State != NotCommitted {
					t.Fatalf("uncommitted child became accepted: %+v", settlement)
				}
				var children int
				if err := f.store.Read(t.Context(), func(tx Executor) error {
					return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM child_jobs`).Scan(&children)
				}); err != nil || children != 0 {
					t.Fatalf("unfinished child survived recovery: %d, %v", children, err)
				}
				if f.calls.Load() != 0 {
					t.Fatal("unaccepted child was dispatched during recovery")
				}
				return
			}
			operation, binding := f.childBinding(t)
			if settlement.State != Committed || operation != string(request) || string(settlement.Evidence) != operation || len(binding.Roots) != 1 || binding.Roots[0] != f.child {
				t.Fatalf("committed child lost its original evidence: %+v; operation=%q binding=%+v", settlement, operation, binding)
			}
			if err := f.host.Uninstall(t.Context(), f.child.InstallationID); !errors.Is(err, frame.ErrInUse) {
				t.Fatalf("recovered child lost durable retention: %v", err)
			}
			caller := frame.NewLifetime(context.Background())
			t.Cleanup(func() { _, _ = caller.Close(context.Background()) })
			scope, err := f.admission.OpenScope(t.Context(), "explicit-child-recovery")
			if err != nil {
				t.Fatal(err)
			}
			use, err := f.host.AcquireRecord(t.Context(), operation, frame.UseConfig{Owner: caller, Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			if err := frame.BorrowExport(t.Context(), use, binding.Roots[0], func(_ context.Context, run func() string) error {
				if run() != "child-provider" {
					t.Fatal("recovery invoked another provider")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if f.calls.Load() != 1 {
				t.Fatalf("explicit original child dispatch count=%d", f.calls.Load())
			}
		})
	}
}

func TestDurableChildCommitCutProcess(t *testing.T) {
	phase := os.Getenv("FRAME_CHILD_CUT")
	if phase == "" {
		return
	}
	f := openChildAcceptance(t, os.Getenv("FRAME_CHILD_PATH"), true)
	request := []byte("durable-child:" + phase)
	id := f.prepare(t, request)
	plan := f.plan(t, f.child)
	cut := childCommitCut{Phase: phase, ID: id}
	pause := func() {
		cut.Calls = f.calls.Load()
		if err := json.NewEncoder(os.Stdout).Encode(cut); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		t.Fatal("parent did not kill the original child commit at its declared cut")
	}
	installTransactionFault(t, f.store, transactionFault{
		beforeExec: func(query string) error {
			if phase == "before-commit" && query == "COMMIT" {
				pause()
			}
			if phase == "commit-unconfirmed" && (query == "COMMIT" || query == "ROLLBACK") {
				return errors.New("child transaction conclusion is unavailable")
			}
			return nil
		},
		afterExec: func(query string) error {
			if phase == "committed-before-response" && query == "COMMIT" {
				pause()
			}
			return nil
		},
		beforeClose: func() error {
			if phase == "commit-unconfirmed" {
				return errors.New("original child transaction connection is still live")
			}
			return nil
		},
	})
	settlement, err := f.owner.Commit(t.Context(), id, request, f.accept(plan, string(request)), nil)
	if phase == "commit-unconfirmed" && errors.Is(err, ErrIndeterminate) && settlement.State == Indeterminate && errors.Is(f.admission.RecoveryError(), frame.ErrRecoveryRequired) {
		cut.Unknown = true
		pause()
	}
	t.Fatalf("child submission returned before its declared cut: %+v, %v", settlement, err)
}
