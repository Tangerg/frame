package document_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
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
	if err != nil || settlement.State != frame.CommitSucceeded {
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
	if err != nil || observed.State != frame.CommitSucceeded {
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
	if !errors.Is(err, frame.ErrStaleBinding) || settlement.State != frame.CommitRejected {
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

func TestBootstrapResumesEachCommittedPrefix(t *testing.T) {
	for installed := 0; installed <= 4; installed++ {
		t.Run(fmt.Sprint(installed), func(t *testing.T) {
			first, config, ctx := application(t)
			if err := first.Close(ctx); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", config.Database)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`DELETE FROM document_bootstrap`); err != nil {
				t.Fatal(err)
			}
			for _, missing := range []string{"files", "converter", "validators", "preview"}[installed:] {
				if _, err = db.Exec(`DELETE FROM frame_installations WHERE id=?`, missing); err != nil {
					t.Fatal(err)
				}
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			restored, err := document.Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close(ctx)
			records, err := restored.Store.Installations(ctx)
			if err != nil || len(records) != 4 {
				t.Fatal(records, err)
			}
		})
	}
}

func TestCompletedBootstrapHonorsLaterUninstall(t *testing.T) {
	first, config, ctx := application(t)
	if err := first.Host.Uninstall(ctx, "preview"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := document.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	if _, present := restored.Host.Installation("preview"); present {
		t.Fatal("uninstalled default resurrected")
	}
}

func TestTaskFinishesWithNoReceiptCapacity(t *testing.T) {
	a, cfg, ctx := application(t)
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cfg.MaxSubmissionsPerOwner = 2
	var err error
	a, err = document.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if _, _, err = a.Queue(ctx, "first", "hello.md"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Queue(ctx, "waiting", "hello.md"); err != nil {
		t.Fatal(err)
	}
	result, err := a.Run(ctx, "first")
	if err != nil || result.State != document.Completed {
		t.Fatal(result, err)
	}
	if _, _, err = a.Queue(ctx, "second", "hello.md"); err != nil {
		t.Fatal(err)
	}
	if err = a.Cancel(ctx, "second"); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitHeadingCanMatchDocumentName(t *testing.T) {
	a, cfg, ctx := application(t)
	if err := os.WriteFile(filepath.Join(cfg.DocumentsDirectory, "hello.md"), []byte("# hello.md"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Queue(ctx, "title", "hello.md"); err != nil {
		t.Fatal(err)
	}
	task, err := a.Run(ctx, "title")
	if err != nil || task.Result == nil || len(task.Result.Diagnostics) != 0 {
		t.Fatal(task, err)
	}
}

func TestHTTPDistinguishesCredentialsAndMissingTasks(t *testing.T) {
	a, _, _ := application(t)
	token := strings.Repeat("a", 32)
	h, err := a.Handler(document.HTTPConfig{ApplicationToken: token})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		auth   string
		status int
		code   string
	}{{token, 401, "unauthenticated"}, {"Bearer " + token, 404, "not_found"}} {
		req := httptest.NewRequest(http.MethodGet, "/api/tasks/missing", nil)
		req.Header.Set("Authorization", tc.auth)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		var body struct {
			Code string
			Task json.RawMessage
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != tc.code || len(body.Task) != 0 || response.Code != tc.status {
			t.Fatal(response.Body, err)
		}
	}
}
