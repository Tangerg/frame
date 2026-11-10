package document_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Tangerg/frame/examples/document"
)

func TestHTTPQueryPreservesOriginalTaskIdentity(t *testing.T) {
	app, _, ctx := application(t)
	token := strings.Repeat("q", 32)
	handler, err := app.Handler(document.HTTPConfig{ApplicationToken: token})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{".", "..", "slash?/%", "文档", "a+b&x=y#fragment"} {
		t.Run(id, func(t *testing.T) {
			if _, _, err := app.Queue(ctx, id, "hello.md"); err != nil {
				t.Fatal(err)
			}
			query := url.Values{"id": {id}}
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/tasks?"+query.Encode(), nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var task document.TaskView
			if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil || response.Code != http.StatusOK || task.ID != id {
				t.Fatalf("original task lookup: status %d, task %+v, error %v", response.Code, task, err)
			}
			if err := app.Cancel(ctx, id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTPQueryRejectsUnboundOrAmbiguousInputs(t *testing.T) {
	app, _, ctx := application(t)
	token := strings.Repeat("q", 32)
	handler, err := app.Handler(document.HTTPConfig{ApplicationToken: token})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/tasks", "/api/tasks?id=first&id=second", "/api/tasks?id=first&other=value",
		"/api/tasks?id=%", "/api/theme?unexpected=value", "/api/contracts?unexpected=value",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid query status %d: %s", response.Code, response.Body)
			}
		})
	}
}
