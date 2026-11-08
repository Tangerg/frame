package browser_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/browser"
)

func TestControlHTTPRejectsInvalidShapeBeforeDispatch(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	credentials, err := e.bridge.OpenRealm(e.ctx, "http-shape", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	e.credentials = credentials
	authority, err := json.Marshal(credentials.Authority)
	if err != nil {
		t.Fatal(err)
	}
	realm, err := json.Marshal(credentials.Realm)
	if err != nil {
		t.Fatal(err)
	}
	reconnect := `{"authority":` + string(authority) + `,"realm":` + string(realm)
	cases := []struct {
		name string
		body string
	}{
		{"null envelope", `null`},
		{"missing method", `{"input":{}}`},
		{"null method", `{"method":null,"input":{}}`},
		{"wrong method type", `{"method":1,"input":{}}`},
		{"missing input", `{"method":"poll"}`},
		{"unknown envelope field", `{"method":"poll","input":{},"extra":true}`},
		{"null poll", `{"method":"poll","input":null}`},
		{"wrong poll type", `{"method":"poll","input":[]}`},
		{"unknown poll field", `{"method":"poll","input":{"extra":true}}`},
		{"missing executions", `{"method":"reconnect","input":` + reconnect + `}}`},
		{"null executions", `{"method":"reconnect","input":` + reconnect + `,"executions":null}}`},
		{"wrong executions type", `{"method":"reconnect","input":` + reconnect + `,"executions":{}}}`},
		{"null execution", `{"method":"reconnect","input":` + reconnect + `,"executions":[null]}}`},
		{"missing authority epoch", `{"method":"reconnect","input":{"authority":{"id":"` + credentials.Authority.ID + `"},"realm":` + string(realm) + `,"executions":[]}}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/control", bytes.NewBufferString(test.body))
			request.Header.Set("Authorization", "Bearer "+credentials.Token)
			response := httptest.NewRecorder()
			e.bridge.Handler().ServeHTTP(response, request)
			var problem browser.HTTPProblem
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusBadRequest || problem.Code != "invalid_plan" {
				t.Fatalf("invalid request admitted: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	request := browser.ReconnectRequest{Authority: credentials.Authority, Realm: credentials.Realm, Executions: []browser.Execution{}}
	response := controlReply(t, e.bridge, credentials.Token, "reconnect", request)
	var result browser.ReconnectResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || result.Mode != browser.Revalidated || len(result.Executions) != 0 {
		t.Fatalf("malformed requests sealed the original Realm: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := controlReply(t, e.bridge, credentials.Token, "poll", struct{}{}); response.Code != http.StatusOK {
		t.Fatalf("valid empty input refused: status=%d body=%s", response.Code, response.Body.String())
	}
}
