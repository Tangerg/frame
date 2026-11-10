package document

import (
	"context"
	"io"
	"net/http"
	"reflect"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
)

type QueueRequest struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type TaskRequest struct {
	ID string `json:"id"`
}

type ConverterRequest struct {
	Version string `json:"version"`
}

type ThemeView struct {
	CSS string `json:"css"`
}

type ContractsView struct {
	Previews frame.ContractRef `json:"previews"`
}

type OpenRealmRequest struct {
	OperationID string `json:"operationId"`
	ResumeToken string `json:"resumeToken"`
}

type ResumeRealmRequest struct {
	Realm       browser.Realm `json:"realm"`
	ResumeToken string        `json:"resumeToken"`
}

type SealRealmRequest struct {
	Token string `json:"token"`
}

type applicationOperation struct {
	contract browser.HTTPOperation
	execute  func(*App, http.ResponseWriter, *http.Request) (any, error)
}

func (o applicationOperation) serve(a *App, writer http.ResponseWriter, request *http.Request) {
	output, err := o.execute(a, writer, request)
	if err != nil {
		writeFailure(writer, output, err)
		return
	}
	writeResponse(writer, o.contract.SuccessStatus, output)
}

func applicationOperations() []applicationOperation {
	return []applicationOperation{
		applicationCommand("queue", "/api/queue", func(a *App, ctx context.Context, input QueueRequest) (TaskView, error) {
			task, _, err := a.Queue(ctx, input.ID, input.Path)
			return view(task), err
		}),
		applicationCommand("run", "/api/run", func(a *App, ctx context.Context, input TaskRequest) (TaskView, error) {
			task, err := a.Run(ctx, input.ID)
			return view(task), err
		}),
		applicationQueryInput("task", "/api/tasks", func(a *App, ctx context.Context, input TaskRequest) (TaskView, error) {
			task, err := a.Task(ctx, input.ID)
			return view(task), err
		}),
		applicationAcknowledgement("cancel", "/api/cancel", func(a *App, ctx context.Context, input TaskRequest) error {
			return a.Cancel(ctx, input.ID)
		}),
		applicationAcknowledgement("replaceConverter", "/api/converter", func(a *App, ctx context.Context, input ConverterRequest) error {
			return a.ReplaceConverter(ctx, input.Version)
		}),
		applicationQuery("theme", "/api/theme", func(a *App, ctx context.Context) (ThemeView, error) {
			theme, err := a.Theme(ctx)
			return ThemeView{CSS: theme}, err
		}),
		applicationQuery("contracts", "/api/contracts", func(a *App, _ context.Context) (ContractsView, error) {
			return ContractsView{Previews: a.Contracts.Previews.Ref()}, nil
		}),
		applicationCommand("openRealm", "/api/realm/open", func(a *App, ctx context.Context, input OpenRealmRequest) (browser.Credentials, error) {
			return a.Bridge.OpenRealm(ctx, input.OperationID, input.ResumeToken)
		}),
		applicationCommand("resumeRealm", "/api/realm/resume", func(a *App, ctx context.Context, input ResumeRealmRequest) (browser.Credentials, error) {
			return a.Bridge.ResumeRealm(ctx, input.Realm, input.ResumeToken)
		}),
		applicationAcknowledgement("sealRealm", "/api/realm/seal", func(a *App, ctx context.Context, input SealRealmRequest) error {
			return a.Bridge.SealRealm(ctx, input.Token)
		}),
		applicationAcknowledgement("activatePreview", "/api/preview/activate", (*App).ActivatePreview),
		applicationAcknowledgement("recover", "/api/recover", func(a *App, ctx context.Context, _ struct{}) error {
			return a.Recover(ctx)
		}),
	}
}

func applicationCommand[I, O any](name, path string, call func(*App, context.Context, I) (O, error)) applicationOperation {
	return applicationOperation{
		contract: browser.HTTPOperation{Name: name, Method: http.MethodPost, Path: path, SuccessStatus: http.StatusOK, Input: reflect.TypeFor[I](), Output: reflect.TypeFor[O]()},
		execute: func(a *App, w http.ResponseWriter, r *http.Request) (any, error) {
			var input I
			if err := decodeRequest(w, r, &input); err != nil {
				return nil, err
			}
			return call(a, r.Context(), input)
		},
	}
}

func applicationAcknowledgement[I any](name, path string, call func(*App, context.Context, I) error) applicationOperation {
	operation := applicationCommand(name, path, func(a *App, ctx context.Context, input I) (any, error) {
		return nil, call(a, ctx, input)
	})
	operation.contract.Output = nil
	return operation
}

func applicationQuery[O any](name, path string, call func(*App, context.Context) (O, error)) applicationOperation {
	return applicationOperation{
		contract: browser.HTTPOperation{Name: name, Method: http.MethodGet, Path: path, SuccessStatus: http.StatusOK, Output: reflect.TypeFor[O]()},
		execute: func(a *App, w http.ResponseWriter, r *http.Request) (any, error) {
			var input struct{}
			if err := decodeQuery(r, &input); err != nil {
				return nil, err
			}
			return call(a, r.Context())
		},
	}
}

func applicationQueryInput[I, O any](name, path string, call func(*App, context.Context, I) (O, error)) applicationOperation {
	return applicationOperation{
		contract: browser.HTTPOperation{Name: name, Method: http.MethodGet, Path: path, SuccessStatus: http.StatusOK, Input: reflect.TypeFor[I](), Output: reflect.TypeFor[O]()},
		execute: func(a *App, w http.ResponseWriter, r *http.Request) (any, error) {
			var input I
			if err := decodeQuery(r, &input); err != nil {
				return nil, err
			}
			return call(a, r.Context(), input)
		},
	}
}

func WriteTypeScriptAPI(out io.Writer) error {
	operations := applicationOperations()
	contracts := make([]browser.HTTPOperation, len(operations))
	for index, operation := range operations {
		contracts[index] = operation.contract
	}
	return browser.WriteTypeScriptHTTP(out, contracts, reflect.TypeFor[PreviewMetadata](), reflect.TypeFor[apiFailure]())
}
