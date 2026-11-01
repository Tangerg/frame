package document_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
	"github.com/Tangerg/frame/sqlite"
)

func application(t *testing.T) (*document.App, document.Config, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	directory := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	})
	documents := filepath.Join(directory, "documents")
	if err := os.Mkdir(documents, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(documents, "hello.md"), []byte("# Original\nHello <world>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := document.Config{Database: filepath.Join(directory, "workbench.db"), ContentDirectory: filepath.Join(directory, "content"), DocumentsDirectory: documents}
	app, err := document.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := app.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return app, config, ctx
}

func TestQueuedBusinessBindingBlocksReplaceAndOriginalResultIsStable(t *testing.T) {
	app, config, ctx := application(t)
	reference, err := app.Reference(ctx, "hello.md")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := app.PrepareTask(ctx, "task-one", reference)
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := app.CommitTask(ctx, prepared)
	if err != nil || settlement.State != sqlite.Committed {
		t.Fatalf("admission: %+v %v", settlement, err)
	}
	if _, err := app.CommitTask(ctx, prepared); err != nil {
		t.Fatalf("lost submission ACK executed another insert: %v", err)
	}
	if err := app.ReplaceConverter(ctx, "2"); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("waiting task did not retain converter: %v", err)
	}
	task, err := app.Run(ctx, "task-one")
	if err != nil || task.State != document.Completed || task.Result == nil {
		t.Fatalf("run: %+v %v", task, err)
	}
	if task.Result.Converted.Title != "Original" || !strings.Contains(task.Result.Converted.HTML, "Hello &lt;world&gt;") {
		t.Fatalf("typed native file import/converter chain: %+v", task.Result)
	}
	if err := app.ReplaceConverter(ctx, "2"); err != nil {
		t.Fatalf("terminal business state retained reference: %v", err)
	}
	if err := os.WriteFile(filepath.Join(config.DocumentsDirectory, "hello.md"), []byte("# Changed"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := app.Run(ctx, "task-one")
	if err != nil || again.Result.Converted.Title != "Original" {
		t.Fatalf("original task was replayed: %+v %v", again, err)
	}
	observed, err := app.ObserveSubmission(ctx, prepared)
	if err != nil || observed.State != sqlite.Committed {
		t.Fatalf("admission history changed with current references: %+v %v", observed, err)
	}
	theme, err := app.Theme(ctx)
	if err != nil || !strings.Contains(theme, "color-scheme") {
		t.Fatalf("static-resource driver: %q %v", theme, err)
	}
}

func TestPreparedCaptureIsRejectedAfterConcurrentDisable(t *testing.T) {
	app, _, ctx := application(t)
	reference, err := app.Reference(ctx, "hello.md")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := app.PrepareTask(ctx, "stale", reference)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Host.SetEnabled(ctx, "converter", false); err != nil {
		t.Fatal(err)
	}
	settlement, err := app.CommitTask(ctx, prepared)
	if !errors.Is(err, frame.ErrStaleBinding) || settlement.State != sqlite.NotCommitted {
		t.Fatalf("stale capture committed: %+v %v", settlement, err)
	}
	if _, err := app.Task(ctx, "stale"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected admission wrote business state: %v", err)
	}
}

func TestQueuedTaskRestoresExactGraphAndRejectsChangedSource(t *testing.T) {
	app, config, ctx := application(t)
	original, receipt, err := app.Queue(ctx, "retained", "hello.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := document.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	again, repeated, err := restored.Queue(ctx, "retained", "hello.md")
	if err != nil || receipt != repeated {
		t.Fatalf("idempotent domain request: %+v %v", repeated, err)
	}
	before, _ := original.Definition.Binding.Identity()
	after, _ := again.Definition.Binding.Identity()
	if before != after {
		t.Fatal("restoration selected a new graph")
	}
	if err := restored.ReplaceConverter(ctx, "2"); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("restored task did not retain exact dependency: %v", err)
	}
	if err := os.WriteFile(filepath.Join(config.DocumentsDirectory, "hello.md"), []byte("# New revision"), 0600); err != nil {
		t.Fatal(err)
	}
	failed, err := restored.Run(ctx, "retained")
	if !errors.Is(err, document.ErrSourceChanged) || failed.State != document.Failed {
		t.Fatalf("changed source was consumed: %+v %v", failed, err)
	}
	if err := restored.ReplaceConverter(ctx, "2"); err != nil {
		t.Fatal(err)
	}
}
