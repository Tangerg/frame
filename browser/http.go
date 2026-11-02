package browser

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
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
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) > 1024 {
			writeProblem(writer, http.StatusUnauthorized, deniedProblem.code, "Realm authentication is required")
			return
		}
		token := strings.TrimPrefix(authorization, "Bearer ")
		if _, err := b.session(token, false); err != nil {
			writeProblem(writer, http.StatusUnauthorized, deniedProblem.code, "Realm authentication failed")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, b.config.MaxRequestBytes))
		if err != nil {
			writeProblem(writer, http.StatusRequestEntityTooLarge, capacityProblem.code, "control request exceeds its byte limit")
			return
		}
		var envelope HTTPEnvelope
		if err := jsonv2.Unmarshal(body, &envelope, jsonv2.RejectUnknownMembers(true)); err != nil {
			writeProblem(writer, http.StatusBadRequest, invalidProblem.code, "invalid control request")
			return
		}
		result, err := b.dispatch(request.Context(), token, envelope)
		if err != nil {
			p := classifyProblem(err)
			writeProblem(writer, p.status, p.code, p.message)
			return
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			writeProblem(writer, http.StatusInternalServerError, uncertainProblem.code, "control response could not be encoded")
			return
		}
		_, _ = writer.Write(encoded)
	})
}

func decodeInput[T any](input json.RawMessage) (T, error) {
	var value T
	err := jsonv2.Unmarshal(input, &value, jsonv2.RejectUnknownMembers(true))
	if err != nil {
		return value, frame.ErrInvalid
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
	_ = json.NewEncoder(writer).Encode(HTTPProblem{Code: code, Message: message})
}
