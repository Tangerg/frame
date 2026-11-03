package document_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
	"github.com/Tangerg/frame/value"
)

func TestCloseJoinsTaskSettlementBeforeClosingItsStore(t *testing.T) {
	app, config, ctx := application(t)
	validating := make(chan struct{})
	validator, err := frame.Define(frame.Definition{
		Revision: value.Hash([]byte("task-settlement-validator")),
		Units: []frame.Unit{{
			Declaration: frame.UnitDeclaration{
				ID: "settlement", Profile: frame.NativeGo,
				Exports: []frame.ExportDeclaration{{Name: "settlement", Contract: app.Contracts.Validators.Ref()}},
			},
			Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
				return []frame.ExportValue{frame.Contribute("settlement", app.Contracts.Validators,
					document.ValidatorMetadata{Name: "settlement", Order: 100},
					document.Validator(func(ctx context.Context, _ document.Converted) ([]document.Diagnostic, error) {
						close(validating)
						<-ctx.Done()
						return nil, nil
					}))}, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Host.Install(ctx, frame.InstallationConfig{ID: "settlement", Plugin: validator, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	const taskID = "settlement-before-close"
	if _, _, err := app.Queue(ctx, taskID, "hello.md"); err != nil {
		t.Fatal(err)
	}
	run := make(chan error, 1)
	go func() {
		task, err := app.Run(ctx, taskID)
		if err == nil && (task.State != document.Completed || task.Result == nil) {
			err = errors.New("task did not preserve its original completed result")
		}
		run <- err
	}()
	select {
	case <-validating:
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	held := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	admitted := make(chan error, 1)
	go func() {
		admitted <- app.Host.Admission().Admit(ctx, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	closing, cancel := context.WithTimeout(ctx, time.Second)
	closeErr := app.Close(closing)
	cancel()
	if !errors.Is(closeErr, frame.ErrUnconfirmedRelease) || !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Errorf("close discarded the outstanding business settlement: %v", closeErr)
	}
	unblock()
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-run:
		if err != nil {
			t.Errorf("original task could not settle: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", config.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var state document.TaskState
	if err := database.QueryRowContext(ctx, `SELECT state FROM document_tasks WHERE id=?`, taskID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != document.Completed {
		t.Fatalf("shutdown left an already completed task in %q", state)
	}
}
