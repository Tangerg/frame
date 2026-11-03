package document

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/sqlite"
)

type HTTPConfig struct {
	ApplicationToken string
	WebDirectory     string
}

// Storage identities travel as strings to preserve integers outside the JavaScript safe range.
type SubmissionView struct {
	Store    string `json:"store"`
	Owner    string `json:"owner"`
	Sequence string `json:"sequence"`
}
type TaskView struct {
	ID         string                `json:"id"`
	Source     DocumentRef           `json:"source"`
	Binding    frame.BindingSnapshot `json:"binding"`
	State      TaskState             `json:"state"`
	RunEpoch   string                `json:"runEpoch"`
	Submission SubmissionView        `json:"submission"`
	Result     *TaskResult           `json:"result,omitempty"`
	Failure    string                `json:"failure,omitempty"`
}

func view(task Task) TaskView {
	return TaskView{ID: task.Definition.ID, Source: task.Definition.Source, Binding: task.Definition.Binding, State: task.State, RunEpoch: strconv.FormatUint(task.RunEpoch, 10), Submission: SubmissionView{Store: task.Submission.Store, Owner: task.Submission.Owner, Sequence: strconv.FormatUint(task.Submission.Sequence, 10)}, Result: task.Result, Failure: task.Failure}
}

const (
	minApplicationTokenBytes = 32
	maxRequestBytes          = 64 << 10
)

// Handler mounts the application policy and original business owner beside the
// framework control handler. It never introduces another provider registry.
func (a *App) Handler(config HTTPConfig) (http.Handler, error) {
	if len(config.ApplicationToken) < minApplicationTokenBytes {
		return nil, fmt.Errorf("document: an application token of at least %d bytes is required", minApplicationTokenBytes)
	}
	mux := http.NewServeMux()
	mux.Handle("/frame/control", a.Bridge.Handler())
	mux.HandleFunc("GET /frame/artifacts/", func(writer http.ResponseWriter, request *http.Request) {
		data, kind, err := a.artifact(request.Context(), strings.TrimPrefix(request.URL.Path, "/frame/artifacts/"))
		if err != nil {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", kind)
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = writer.Write(data)
	})
	api := http.NewServeMux()
	api.HandleFunc("POST /api/queue", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		}
		if !decodeRequest(writer, request, &input) {
			return
		}
		task, _, err := a.Queue(request.Context(), input.ID, input.Path)
		respond(writer, task, err)
	})
	api.HandleFunc("POST /api/run", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			ID string `json:"id"`
		}
		if !decodeRequest(writer, request, &input) {
			return
		}
		task, err := a.Run(request.Context(), input.ID)
		respond(writer, task, err)
	})
	api.HandleFunc("GET /api/tasks/{id}", func(writer http.ResponseWriter, request *http.Request) {
		task, err := a.Task(request.Context(), request.PathValue("id"))
		respond(writer, task, err)
	})
	api.HandleFunc("POST /api/cancel", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			ID string `json:"id"`
		}
		if !decodeRequest(writer, request, &input) {
			return
		}
		respond(writer, nil, a.Cancel(request.Context(), input.ID))
	})
	api.HandleFunc("POST /api/converter", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Version string `json:"version"`
		}
		if !decodeRequest(writer, request, &input) {
			return
		}
		respond(writer, nil, a.ReplaceConverter(request.Context(), input.Version))
	})
	api.HandleFunc("GET /api/theme", func(writer http.ResponseWriter, request *http.Request) {
		theme, err := a.Theme(request.Context())
		respond(writer, struct {
			CSS string `json:"css"`
		}{theme}, err)
	})
	api.HandleFunc("GET /api/contracts", func(writer http.ResponseWriter, _ *http.Request) {
		respond(writer, struct {
			Previews frame.ContractRef `json:"previews"`
		}{a.Contracts.Previews.Ref()}, nil)
	})
	api.HandleFunc("POST /api/realm/open", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			OperationID string `json:"operationId"`
			ResumeToken string `json:"resumeToken"`
		}
		if !decodeRequest(writer, request, &input) {
			return
		}
		credentials, err := a.Bridge.OpenRealm(request.Context(), input.OperationID, input.ResumeToken)
		respond(writer, credentials, err)
	})
	api.HandleFunc("POST /api/realm/resume", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Realm       browser.Realm `json:"realm"`
			ResumeToken string        `json:"resumeToken"`
		}
		if !decodeRequest(writer, request, &input) {
			return
		}
		credentials, err := a.Bridge.ResumeRealm(request.Context(), input.Realm, input.ResumeToken)
		respond(writer, credentials, err)
	})
	api.HandleFunc("POST /api/realm/seal", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Token string `json:"token"`
		}
		if !decodeRequest(writer, request, &input) {
			return
		}
		respond(writer, nil, a.Bridge.SealRealm(request.Context(), input.Token))
	})
	api.HandleFunc("POST /api/preview/activate", func(writer http.ResponseWriter, request *http.Request) {
		var credentials browser.Credentials
		if !decodeRequest(writer, request, &credentials) {
			return
		}
		respond(writer, nil, a.ActivatePreview(request.Context(), credentials))
	})
	api.HandleFunc("POST /api/recover", func(writer http.ResponseWriter, request *http.Request) {
		respond(writer, nil, a.Recover(request.Context()))
	})
	mux.Handle("/api/", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		scheme, received, ok := strings.Cut(request.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(received), []byte(config.ApplicationToken)) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			respond(writer, nil, errAuthentication)
			return
		}
		api.ServeHTTP(writer, request)
	}))
	if config.WebDirectory != "" {
		mux.Handle("/", http.FileServerFS(os.DirFS(config.WebDirectory)))
	}
	return mux, nil
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, target any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			err = frame.ErrCapacity
		}
		respond(writer, nil, err)
		return false
	}
	if err := jsonv2.Unmarshal(data, target, jsonv2.RejectUnknownMembers(true)); err != nil {
		respond(writer, nil, frame.ErrInvalid)
		return false
	}
	return true
}

var errAuthentication = errors.New("document: authentication required")

type apiFailure struct {
	Code    string    `json:"code"`
	Message string    `json:"error"`
	Task    *TaskView `json:"task,omitempty"`
}

func describeError(err error) (int, apiFailure) {
	switch {
	case errors.Is(err, errAuthentication):
		return http.StatusUnauthorized, apiFailure{Code: "unauthenticated", Message: "application authentication required"}
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound, apiFailure{Code: "not_found", Message: "resource not found"}
	case errors.Is(err, frame.ErrCapacity):
		return http.StatusRequestEntityTooLarge, apiFailure{Code: "capacity", Message: "request exceeds the available capacity"}
	case errors.Is(err, frame.ErrInvalid):
		return http.StatusBadRequest, apiFailure{Code: "invalid_request", Message: "invalid request"}
	case errors.Is(err, frame.ErrFenced):
		return http.StatusForbidden, apiFailure{Code: "fenced", Message: "authorization is no longer valid"}
	case errors.Is(err, frame.ErrRecoveryRequired), errors.Is(err, sqlite.ErrIndeterminate):
		return http.StatusServiceUnavailable, apiFailure{Code: "recovery_required", Message: "observe the original operation after recovery"}
	case errors.Is(err, ErrTaskFinished), errors.Is(err, ErrSourceChanged), errors.Is(err, sqlite.ErrConflict), errors.Is(err, frame.ErrInUse), errors.Is(err, frame.ErrStaleBinding), errors.Is(err, frame.ErrUnavailable):
		return http.StatusConflict, apiFailure{Code: "conflict", Message: err.Error()}
	default:
		return http.StatusInternalServerError, apiFailure{Code: "internal", Message: "application operation failed"}
	}
}

func respond(writer http.ResponseWriter, value any, err error) {
	writer.Header().Set("Content-Type", "application/json")
	if err != nil {
		status, failure := describeError(err)
		if task, ok := value.(Task); ok && task.Definition.ID != "" {
			detail := view(task)
			failure.Task = &detail
		}
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(failure)
		return
	}
	if task, ok := value.(Task); ok {
		value = view(task)
	}
	_ = json.NewEncoder(writer).Encode(value)
}
