package document

import (
	"crypto/subtle"
	"database/sql"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
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
	ID         string                    `json:"id"`
	Source     DocumentRef               `json:"source"`
	Binding    framehost.BindingSnapshot `json:"binding"`
	State      TaskState                 `json:"state"`
	RunStatus  TaskRunStatus             `json:"runStatus,omitempty"`
	RunEpoch   string                    `json:"runEpoch"`
	Submission SubmissionView            `json:"submission"`
	Result     *TaskResult               `json:"result,omitempty"`
	Failure    string                    `json:"failure,omitempty"`
}

func view(task Task) TaskView {
	return TaskView{ID: task.Definition.ID, Source: task.Definition.Source, Binding: task.Definition.Binding, State: task.State, RunStatus: task.RunStatus, RunEpoch: strconv.FormatUint(task.RunEpoch, 10), Submission: SubmissionView{Store: task.Submission.Store, Owner: task.Submission.Owner, Sequence: strconv.FormatUint(task.Submission.Sequence, 10)}, Result: task.Result, Failure: task.Failure}
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
	mux.HandleFunc(http.MethodGet+" "+artifactPathPrefix, func(writer http.ResponseWriter, request *http.Request) {
		data, kind, err := a.artifact(request.Context(), strings.TrimPrefix(request.URL.Path, artifactPathPrefix))
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
	for _, operation := range applicationOperations() {
		api.HandleFunc(operation.contract.Method+" "+operation.contract.Path, func(writer http.ResponseWriter, request *http.Request) {
			operation.serve(a, writer, request)
		})
	}
	mux.Handle("/api/", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		scheme, received, ok := strings.Cut(request.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(received), []byte(config.ApplicationToken)) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writeFailure(writer, nil, errAuthentication)
			return
		}
		api.ServeHTTP(writer, request)
	}))
	if config.WebDirectory != "" {
		mux.Handle("/", http.FileServerFS(os.DirFS(config.WebDirectory)))
	}
	return mux, nil
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, target any) error {
	data, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return errors.Join(frame.ErrCapacity, err)
		}
		return err
	}
	if err := value.DecodeValue(data, target, "application request"); err != nil {
		return errors.Join(frame.ErrInvalid, err)
	}
	return nil
}

func decodeQuery(request *http.Request, target any) error {
	if len(request.URL.RawQuery) > maxRequestBytes {
		return frame.ErrCapacity
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return errors.Join(frame.ErrInvalid, err)
	}
	members := make(map[string]string, len(query))
	for name, values := range query {
		if len(values) != 1 {
			return frame.ErrInvalid
		}
		members[name] = values[0]
	}
	data, err := jsonv2.Marshal(members)
	if err == nil {
		err = value.DecodeValue(data, target, "application query")
	}
	if err != nil {
		return errors.Join(frame.ErrInvalid, err)
	}
	return nil
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
	case errors.Is(err, framehost.ErrFenced):
		return http.StatusForbidden, apiFailure{Code: "fenced", Message: "authorization is no longer valid"}
	case errors.Is(err, framehost.ErrRecoveryRequired), errors.Is(err, sqlite.ErrIndeterminate):
		return http.StatusServiceUnavailable, apiFailure{Code: "recovery_required", Message: "observe the original operation after recovery"}
	case errors.Is(err, ErrTaskFinished), errors.Is(err, ErrTaskOutcomeUnknown), errors.Is(err, ErrSourceChanged), errors.Is(err, sqlite.ErrConflict), errors.Is(err, framehost.ErrInUse), errors.Is(err, framehost.ErrStaleBinding), errors.Is(err, framehost.ErrUnavailable):
		return http.StatusConflict, apiFailure{Code: "conflict", Message: err.Error()}
	default:
		return http.StatusInternalServerError, apiFailure{Code: "internal", Message: "application operation failed"}
	}
}

func writeFailure(writer http.ResponseWriter, output any, err error) {
	status, failure := describeError(err)
	if task, ok := output.(TaskView); ok && task.ID != "" {
		failure.Task = &task
	}
	writeResponse(writer, status, failure)
}

func writeResponse(writer http.ResponseWriter, status int, output any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = jsonv2.MarshalWrite(writer, output)
}
