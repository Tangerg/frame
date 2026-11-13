package document_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func installRunValidator(t *testing.T, app *document.App, validate document.Validator) {
	t.Helper()
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash([]byte("original-run-validator")), Units: []framehost.Unit{{
		Declaration: framehost.UnitDeclaration{ID: "validator", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "validator", Contract: app.Contracts.Validators.Ref()}}},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("validator", app.Contracts.Validators, document.ValidatorMetadata{Name: "original-run", Order: 100}, validate)}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Host.Install(t.Context(), framehost.InstallationConfig{ID: "original-run-validator", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedTaskCanBeSettledAfterItsOriginalRunExits(t *testing.T) {
	panicCause := errors.New("validator interrupted")
	for _, interruption := range []struct {
		name string
		run  func()
		want any
	}{
		{"panic", func() { panic(panicCause) }, panicCause},
		{"goexit", runtime.Goexit, nil},
	} {
		t.Run(interruption.name, func(t *testing.T) {
			app, _, ctx := application(t)
			var calls atomic.Int32
			installRunValidator(t, app, func(context.Context, document.Converted) ([]document.Diagnostic, error) {
				calls.Add(1)
				interruption.run()
				return nil, nil
			})
			const id = "interrupted-task"
			if _, _, err := app.Queue(ctx, id, "hello.md"); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			recovered := make(chan any, 1)
			go func() {
				defer close(done)
				defer func() { recovered <- recover() }()
				_, _ = app.Run(ctx, id)
			}()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if got := <-recovered; got != interruption.want {
				t.Fatalf("interruption propagation = %v, want %v", got, interruption.want)
			}
			task, err := app.Task(ctx, id)
			if err != nil || task.State != document.Unsettled || task.RunStatus != document.TaskOutcomeUnknown {
				t.Fatalf("original execution exit: %+v, %v", task, err)
			}
			if _, err := app.Run(ctx, id); !errors.Is(err, document.ErrTaskOutcomeUnknown) || calls.Load() != 1 {
				t.Fatalf("replayed an unknown outcome: calls=%d, %v", calls.Load(), err)
			}
			closing, cancel := context.WithCancel(ctx)
			cancel()
			if err := app.Close(closing); !errors.Is(err, frame.ErrUnconfirmedRelease) {
				t.Fatalf("shutdown discarded pending settlement: %v", err)
			}
			if err := app.Cancel(ctx, id); err != nil {
				t.Fatalf("original run cannot settle after shutdown starts: %v", err)
			}
			task, err = app.Task(ctx, id)
			if err != nil || task.State != document.Cancelled || task.RunStatus != "" {
				t.Fatalf("explicit cancellation: %+v, %v", task, err)
			}
			if err := app.Close(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActiveTaskDoesNotAcceptCancellationAsExitEvidence(t *testing.T) {
	app, _, ctx := application(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	installRunValidator(t, app, func(context.Context, document.Converted) ([]document.Diagnostic, error) {
		close(entered)
		<-release
		return nil, nil
	})
	const id = "active-task"
	if _, _, err := app.Queue(ctx, id, "hello.md"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := app.Run(ctx, id); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	task, err := app.Task(ctx, id)
	if err != nil || task.State != document.Unsettled || task.RunStatus != document.TaskRunActive {
		t.Fatalf("active task projection: %+v, %v", task, err)
	}
	if err := app.Cancel(ctx, id); !errors.Is(err, framehost.ErrInUse) {
		t.Fatalf("cancel declared an active callback stopped: %v", err)
	}
	if _, err := app.Run(ctx, id); !errors.Is(err, framehost.ErrInUse) {
		t.Fatalf("repeated an active execution: %v", err)
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestTaskRetainsItsKnownConclusionUntilPersistenceRecovers(t *testing.T) {
	businessFailure := errors.New("original validation refused the document")
	for _, scenario := range []struct {
		name   string
		settle string
		cause  error
	}{
		{"retry run", "run", nil},
		{"explicit cancel", "cancel", nil},
		{"retry close", "close", nil},
		{"known failure", "run", businessFailure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			app, config, ctx := application(t)
			var calls atomic.Int32
			installRunValidator(t, app, func(context.Context, document.Converted) ([]document.Diagnostic, error) {
				calls.Add(1)
				return nil, scenario.cause
			})
			const id = "settlement-pending"
			if _, _, err := app.Queue(ctx, id, "hello.md"); err != nil {
				t.Fatal(err)
			}
			database, err := sql.Open("sqlite", config.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if _, err := database.ExecContext(ctx, `CREATE TRIGGER refuse_task_settlement BEFORE UPDATE OF state ON document_tasks WHEN NEW.state IN ('completed','failed') BEGIN SELECT RAISE(ABORT,'settlement unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			task, err := app.Run(ctx, id)
			if err == nil || task.State != document.Unsettled || task.RunStatus != document.TaskSettlementPending {
				t.Fatalf("known result lost after failed persistence: %+v, %v", task, err)
			}
			if scenario.cause == nil {
				if task.Result == nil || task.Result.Converted.Title != "Original" {
					t.Fatalf("known result changed: %+v", task)
				}
			} else if !errors.Is(err, scenario.cause) || task.Failure != scenario.cause.Error() || task.Result != nil {
				t.Fatalf("known failure changed: %+v, %v", task, err)
			}
			closing, cancel := context.WithCancel(ctx)
			cancel()
			if err := app.Close(closing); !errors.Is(err, frame.ErrUnconfirmedRelease) {
				t.Fatalf("shutdown discarded the known result: %v", err)
			}
			if _, err := database.ExecContext(ctx, `DROP TRIGGER refuse_task_settlement`); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(config.DocumentsDirectory, "hello.md"), []byte("# Changed after original execution"), 0600); err != nil {
				t.Fatal(err)
			}
			switch scenario.settle {
			case "run":
				_, err = app.Run(ctx, id)
			case "cancel":
				err = app.Cancel(ctx, id)
			case "close":
				err = app.Close(ctx)
			}
			if !errors.Is(err, scenario.cause) {
				t.Fatalf("could not settle original result after shutdown began: %v", err)
			}
			var state document.TaskState
			var result []byte
			if err := database.QueryRowContext(ctx, `SELECT state,result FROM document_tasks WHERE id=?`, id).Scan(&state, &result); err != nil {
				t.Fatal(err)
			}
			want := document.Completed
			if scenario.cause != nil {
				want = document.Failed
			}
			if state != want || (len(result) != 0) != (scenario.cause == nil) || calls.Load() != 1 {
				t.Fatalf("original result replaced or execution replayed: state=%s, result=%s, calls=%d", state, result, calls.Load())
			}
		})
	}
}

func TestRecoveredUnsettledTaskRequiresAnExplicitDecision(t *testing.T) {
	app, config, ctx := application(t)
	const id = "recovered-unsettled"
	if _, _, err := app.Queue(ctx, id, "hello.md"); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", config.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.ExecContext(ctx, `UPDATE document_tasks SET state=?,run_epoch=? WHERE id=?`, document.Unsettled, app.Store.Epoch(), id); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := document.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(context.Background())
	task, err := restored.Run(ctx, id)
	if !errors.Is(err, document.ErrTaskOutcomeUnknown) || task.State != document.Unsettled || task.RunStatus != document.TaskOutcomeUnknown {
		t.Fatalf("recovered task replayed or acquired a result: %+v, %v", task, err)
	}
	if err := restored.Cancel(ctx, id); err != nil {
		t.Fatalf("recovered original task could not be explicitly settled: %v", err)
	}
}
