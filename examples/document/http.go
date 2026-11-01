package document

import (
	"crypto/subtle"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/control"
	controlhost "github.com/Tangerg/frame/control/host"
)

type HTTPConfig struct {
	ApplicationToken string
	WebDirectory     string
}

// TaskView is the application's browser projection. Numeric storage identities
// remain decimal strings; they are never rounded through JavaScript numbers.
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

// Handler mounts the application policy and original business owner beside the
// framework control handler. It never introduces another provider registry.
func (a *App) Handler(config HTTPConfig) (http.Handler, error) {
	if len(config.ApplicationToken) < 32 {
		return nil, errors.New("document: an application token of at least 32 bytes is required")
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
			Realm       control.Realm `json:"realm"`
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
		var credentials controlhost.Credentials
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
		received := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(received), []byte(config.ApplicationToken)) != 1 {
			http.Error(writer, "application authentication required", http.StatusUnauthorized)
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
	data, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 64<<10))
	if err != nil {
		respond(writer, nil, frame.ErrCapacity)
		return false
	}
	if err := jsonv2.Unmarshal(data, target, jsonv2.RejectUnknownMembers(true)); err != nil {
		respond(writer, nil, frame.ErrInvalid)
		return false
	}
	return true
}
func respond(writer http.ResponseWriter, value any, err error) {
	if task, ok := value.(Task); ok {
		value = view(task)
	}
	writer.Header().Set("Content-Type", "application/json")
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, frame.ErrCapacity) {
			status = http.StatusRequestEntityTooLarge
		}
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(struct {
			Error string `json:"error"`
			Task  any    `json:"task,omitempty"`
		}{err.Error(), value})
		return
	}
	_ = json.NewEncoder(writer).Encode(value)
}
