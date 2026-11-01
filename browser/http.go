package browser

import (
	"context"
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
			writeProblem(writer, http.StatusMethodNotAllowed, "invalid_plan", "POST is required")
			return
		}
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) > 1024 {
			writeProblem(writer, http.StatusUnauthorized, "permission_denied", "Realm authentication is required")
			return
		}
		token := strings.TrimPrefix(authorization, "Bearer ")
		if _, err := b.session(token, false); err != nil {
			writeProblem(writer, http.StatusUnauthorized, "permission_denied", "Realm authentication failed")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, b.config.MaxRequestBytes))
		if err != nil {
			writeProblem(writer, http.StatusRequestEntityTooLarge, "capacity", "control request exceeds its byte limit")
			return
		}
		var envelope HTTPEnvelope
		if err := jsonv2.Unmarshal(body, &envelope, jsonv2.RejectUnknownMembers(true)); err != nil {
			writeProblem(writer, http.StatusBadRequest, "invalid_plan", "invalid control request")
			return
		}
		result, err := b.dispatch(request.Context(), token, envelope)
		if err != nil {
			status, code := errorStatus(err)
			writeProblem(writer, status, code, publicMessage(code))
			return
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			writeProblem(writer, http.StatusInternalServerError, "outcome_unknown", "control response could not be encoded")
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
func (b *Bridge) dispatch(ctx context.Context, token string, request HTTPEnvelope) (any, error) {
	switch request.Method {
	case "validatePlan":
		value, err := decodeInput[LocalPlan](request.Input)
		if err != nil {
			return nil, err
		}
		return nil, b.ValidatePlan(ctx, token, value)
	case "ready":
		value, err := decodeInput[ReadyReport](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Ready(ctx, token, value)
	case "acquire":
		value, err := decodeInput[AcquireRequest](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Acquire(ctx, token, value)
	case "validateUse":
		value, err := decodeInput[UseGrant](request.Input)
		if err != nil {
			return nil, err
		}
		return nil, b.ValidateUse(ctx, token, value)
	case "validateRoute":
		value, err := decodeInput[RouteGrant](request.Input)
		if err != nil {
			return nil, err
		}
		return nil, b.ValidateRoute(ctx, token, value)
	case "snapshot":
		value, err := decodeInput[[]string](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Snapshot(ctx, token, value)
	case "closeSnapshot":
		value, err := decodeInput[string](request.Input)
		if err != nil {
			return nil, err
		}
		return nil, b.CloseSnapshot(ctx, token, value)
	case "publish":
		value, err := decodeInput[PublicationRequest](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Publish(ctx, token, value)
	case "publication":
		value, err := decodeInput[string](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Publication(ctx, token, value)
	case "withdraw":
		value, err := decodeInput[Withdrawal](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Withdraw(ctx, token, value)
	case "prove":
		value, err := decodeInput[CloseProof](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Prove(ctx, token, value)
	case "reconnect":
		value, err := decodeInput[ReconnectRequest](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Reconnect(ctx, token, value)
	case "poll":
		_, err := decodeInput[struct{}](request.Input)
		if err != nil {
			return nil, err
		}
		return b.Poll(ctx, token)
	default:
		return nil, frame.ErrInvalid
	}
}

func errorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, frame.ErrFenced):
		return http.StatusForbidden, "permission_denied"
	case errors.Is(err, frame.ErrContractMismatch):
		return http.StatusConflict, "contract_mismatch"
	case errors.Is(err, frame.ErrStaleBinding):
		return http.StatusConflict, "stale_binding"
	case errors.Is(err, frame.ErrCapacity):
		return http.StatusTooManyRequests, "capacity"
	case errors.Is(err, frame.ErrClosed):
		return http.StatusConflict, "closed"
	case errors.Is(err, frame.ErrInvalid), errors.Is(err, sqlite.ErrConflict):
		return http.StatusBadRequest, "invalid_plan"
	case errors.Is(err, frame.ErrUnconfirmedRelease):
		return http.StatusConflict, "close_unconfirmed"
	case errors.Is(err, sqlite.ErrIndeterminate):
		return http.StatusServiceUnavailable, "outcome_unknown"
	default:
		return http.StatusServiceUnavailable, "unavailable"
	}
}
func publicMessage(code string) string {
	switch code {
	case "permission_denied":
		return "the original Realm is not authorized for this operation"
	case "stale_binding":
		return "the original binding is no longer current"
	case "contract_mismatch":
		return "the exact contract does not match"
	case "capacity":
		return "the configured capacity is exhausted"
	case "closed":
		return "the original owner is closed"
	case "invalid_plan":
		return "the control request does not match its original identity"
	case "close_unconfirmed":
		return "the original exit has not yet been confirmed"
	case "outcome_unknown":
		return "observe the original operation to resolve its outcome"
	default:
		return "the original control operation is unavailable"
	}
}
func writeProblem(writer http.ResponseWriter, status int, code, message string) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(HTTPProblem{Code: code, Message: message})
}
