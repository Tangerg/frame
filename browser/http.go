package browser

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
	framevalue "github.com/Tangerg/frame/value"
)

// Handler exposes only an authenticated Realm's control port. The application
// owns bootstrap authentication, TLS, origin policy and the URL where it mounts
// this handler; no plugin can install a package or mint Realm credentials here.
func (b *Bridge) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			writeProblem(writer, http.StatusMethodNotAllowed, invalidProblem.code, "POST is required")
			return
		}
		token, bearer := strings.CutPrefix(request.Header.Get(authorizationHeader), bearerScheme)
		if !bearer || len(token) > maxTokenBytes {
			writeProblem(writer, http.StatusUnauthorized, deniedProblem.code, "Realm authentication is required")
			return
		}
		if _, err := b.anySession(token); err != nil {
			writeProblem(writer, http.StatusUnauthorized, deniedProblem.code, "Realm authentication failed")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, b.config.MaxMessageBytes))
		if err != nil {
			writeProblem(writer, http.StatusRequestEntityTooLarge, capacityProblem.code, "control request exceeds its byte limit")
			return
		}
		var envelope HTTPEnvelope
		if err := framevalue.DecodeValue(body, &envelope, "control request"); err != nil {
			writeProblem(writer, http.StatusBadRequest, invalidProblem.code, "invalid control request")
			return
		}
		result, err := b.dispatch(request.Context(), token, envelope)
		if err != nil {
			p := classifyProblem(err)
			writeProblem(writer, p.status, p.code, p.message)
			return
		}
		encoded, err := marshalControl(result)
		if err != nil {
			writeProblem(writer, http.StatusInternalServerError, uncertainProblem.code, "control response could not be encoded")
			return
		}
		if int64(len(encoded)) > b.config.MaxMessageBytes {
			writeProblem(writer, http.StatusServiceUnavailable, uncertainProblem.code, "control response exceeds its byte limit")
			return
		}
		_, _ = writer.Write(encoded)
	})
}

// A Realm token is short, so a longer Authorization header is refused before
// any signature work.
const (
	authorizationHeader = "Authorization"
	bearerScheme        = "Bearer "
	maxTokenBytes       = 1024
)

func decodeInput[T any](input json.RawMessage) (T, error) {
	var value T
	err := framevalue.DecodeValue(input, &value, "control input")
	if err != nil {
		return value, errors.Join(frame.ErrInvalid, err)
	}
	return value, nil
}

type httpProblemKind struct {
	code, message string
	status        int
	causes        []error
}

var (
	deniedProblem      = httpProblemKind{"permission_denied", "the original Realm is not authorized for this operation", http.StatusForbidden, []error{frame.ErrFenced}}
	invalidProblem     = httpProblemKind{"invalid_plan", "the control request does not match its original identity", http.StatusBadRequest, []error{frame.ErrInvalid, sqlite.ErrConflict}}
	capacityProblem    = httpProblemKind{"capacity", "the configured capacity is exhausted", http.StatusTooManyRequests, []error{frame.ErrCapacity}}
	uncertainProblem   = httpProblemKind{"outcome_unknown", "observe the original operation to resolve its outcome", http.StatusServiceUnavailable, []error{sqlite.ErrIndeterminate}}
	unavailableProblem = httpProblemKind{"unavailable", "the original control operation is unavailable", http.StatusServiceUnavailable, nil}
	controlProblems    = []httpProblemKind{
		deniedProblem, invalidProblem, capacityProblem, uncertainProblem,
		{"contract_mismatch", "the exact contract does not match", http.StatusConflict, []error{frame.ErrContractMismatch}},
		{"stale_binding", "the original binding is no longer current", http.StatusConflict, []error{frame.ErrStaleBinding}},
		{"closed", "the original owner is closed", http.StatusConflict, []error{frame.ErrClosed}},
		{"close_unconfirmed", "the original exit has not yet been confirmed", http.StatusConflict, []error{frame.ErrUnconfirmedRelease}}, unavailableProblem,
	}
)

func classifyProblem(err error) httpProblemKind {
	// A secondary failure cannot turn a dispatched operation into a known refusal.
	if errors.Is(err, sqlite.ErrIndeterminate) {
		return uncertainProblem
	}
	for _, p := range controlProblems {
		for _, cause := range p.causes {
			if errors.Is(err, cause) {
				return p
			}
		}
	}
	return unavailableProblem
}
func writeProblem(writer http.ResponseWriter, status int, code, message string) {
	writer.WriteHeader(status)
	_, _ = writer.Write(wireBytes(HTTPProblem{Code: code, Message: message}))
}
