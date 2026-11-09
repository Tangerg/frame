package browser

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Tangerg/frame"
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

func writeProblem(writer http.ResponseWriter, status int, code, message string) {
	writer.WriteHeader(status)
	_, _ = writer.Write(wireBytes(HTTPProblem{Code: code, Message: message}))
}
